// Copyright © 2022 Attestant Limited.
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package standard

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	builderclient "github.com/attestantio/go-builder-client"
	"github.com/attestantio/go-builder-client/api"
	apiv1 "github.com/attestantio/go-builder-client/api/v1"
	"github.com/attestantio/go-builder-client/spec"
	"github.com/attestantio/go-eth2-client/spec/phase0"
	"github.com/attestantio/vouch/services/metrics"
	"github.com/attestantio/vouch/util"
	"github.com/pkg/errors"
	e2types "github.com/wealdtech/go-eth2-types/v2"
	e2wtypes "github.com/wealdtech/go-eth2-wallet-types/v2"
)

func (s *Service) submitValidatorRegistrationsRuntime(_ context.Context,
	_ interface{},
) (
	time.Time,
	error,
) {
	// Schedule for the middle of the slot, half-way through the epoch.
	currentEpoch := s.chainTime.CurrentEpoch()
	epochDuration := s.chainTime.StartOfEpoch(currentEpoch + 1).Sub(s.chainTime.StartOfEpoch(currentEpoch))
	currentSlot := s.chainTime.CurrentSlot()
	slotDuration := s.chainTime.StartOfSlot(currentSlot + 1).Sub(s.chainTime.StartOfSlot(currentSlot))
	offset := int(epochDuration.Seconds()/2.0 + slotDuration.Seconds()/2.0)
	return s.chainTime.StartOfEpoch(s.chainTime.CurrentEpoch() + 1).Add(time.Duration(offset) * time.Second), nil
}

// SubmitValidatorRegistrations submits validator registrations for the given accounts.
func (s *Service) SubmitValidatorRegistrations(ctx context.Context,
	accounts map[phase0.ValidatorIndex]e2wtypes.Account,
) error {
	return s.submitValidatorRegistrationsForAccounts(ctx, accounts)
}

// submitValidatorRegistrationsRuntime sets the runtime for the next validator registrations call.
// submitValidatorRegistrations submits validator registrations.
func (s *Service) submitValidatorRegistrations(ctx context.Context,
	_ interface{},
) {
	started := time.Now()

	epoch := s.chainTime.CurrentEpoch()

	// Fetch the validating accounts for the next epoch, to ensure that we capture any validators
	// that are going to start proposing soon.
	// Note that this will result in us not obtaining a validator that is on its last validating
	// epoch, however preparations linger for a couple of epochs after registration so this is safe.
	accounts, err := s.validatingAccountsProvider.ValidatingAccountsForEpoch(ctx, epoch+1)
	if err != nil {
		monitorValidatorRegistrations(false, time.Since(started))
		log.Error().Err(err).Msg("Failed to obtain validating accounts")
		return
	}
	log.Trace().Dur("elapsed", time.Since(started)).Msg("Obtained validating accounts")

	if len(accounts) == 0 {
		monitorValidatorRegistrations(false, time.Since(started))
		log.Debug().Msg("No validating accounts; not submiting validator registrations")
		return
	}
	if s.boostConfig == nil {
		monitorValidatorRegistrations(false, time.Since(started))
		log.Debug().Msg("No boost config; not submiting validator registrations")
		return
	}

	if err := s.submitValidatorRegistrationsForAccounts(ctx, accounts); err != nil {
		log.Error().Err(err).Msg("Failed to submit validator registrations")
	}

	monitorValidatorRegistrations(true, time.Since(started))
}

func (s *Service) submitValidatorRegistrationsForAccounts(ctx context.Context,
	accounts map[phase0.ValidatorIndex]e2wtypes.Account,
) error {
	if s.boostConfig == nil {
		return errors.New("no boost configuration; cannot submit validator registrations at current")
	}

	signedRegistrations := make(map[string][]*api.VersionedSignedValidatorRegistration)
	var pubkey phase0.BLSPubKey
	for index, account := range accounts {
		var accountPubkey e2types.PublicKey
		if provider, isProvider := account.(e2wtypes.AccountCompositePublicKeyProvider); isProvider {
			accountPubkey = provider.CompositePublicKey()
		} else {
			accountPubkey = account.(e2wtypes.AccountPublicKeyProvider).PublicKey()
		}
		copy(pubkey[:], accountPubkey.Marshal())

		proposerConfig, exists := s.boostConfig.ProposerConfigs[pubkey]
		if !exists {
			proposerConfig = s.boostConfig.DefaultConfig
		}

		registration := &apiv1.ValidatorRegistration{
			FeeRecipient: proposerConfig.FeeRecipient,
			GasLimit:     proposerConfig.Builder.GasLimit,
			Timestamp:    time.Now().Round(time.Second),
			Pubkey:       pubkey,
		}

		sig, err := s.validatorRegistrationSigner.SignValidatorRegistration(ctx, account, &api.VersionedValidatorRegistration{
			Version: spec.BuilderVersionV1,
			V1:      registration,
		})
		if err != nil {
			// Log an error but continue.
			log.Error().Err(err).Uint64("index", uint64(index)).Msg("Failed to sign validator registration")
			continue
		}

		signedRegistration := &apiv1.SignedValidatorRegistration{
			Message:   registration,
			Signature: sig,
		}

		versionedSignedRegistration := &api.VersionedSignedValidatorRegistration{
			Version: spec.BuilderVersionV1,
			V1:      signedRegistration,
		}
		for _, relay := range proposerConfig.Builder.Relays {
			if _, exists := signedRegistrations[relay]; !exists {
				signedRegistrations[relay] = make([]*api.VersionedSignedValidatorRegistration, 0)
			}
			signedRegistrations[relay] = append(signedRegistrations[relay], versionedSignedRegistration)
		}
	}

	if e := log.Trace(); e.Enabled() {
		data, err := json.Marshal(signedRegistrations)
		if err == nil {
			e.RawJSON("registrations", data).Msg("Generated registrations")
		}
	}

	// Submit registrations in parallel to the builders.
	var wg sync.WaitGroup
	for builder, providerRegistrations := range signedRegistrations {
		wg.Add(1)
		go func(ctx context.Context, builder string, providerRegistrations []*api.VersionedSignedValidatorRegistration, monitor metrics.Service) {
			defer wg.Done()
			client, err := util.FetchBuilderClient(ctx, builder, s.monitor)
			if err != nil {
				log.Error().Err(err).Str("builder", builder).Msg("Failed to fetch builder client")
				return
			}
			submitter, isSubmitter := client.(builderclient.ValidatorRegistrationsSubmitter)
			if !isSubmitter {
				log.Error().Str("builder", builder).Msg("Builder client does not accept validator registrations")
				return
			}
			if err := submitter.SubmitValidatorRegistrations(ctx, providerRegistrations); err != nil {
				log.Error().Err(err).Str("builder", builder).Msg("Failed to submit validator registrations")
				return
			}
		}(ctx, builder, providerRegistrations, s.monitor)
	}

	//	// Submit registrations in parallel to the builders.
	//	var wg sync.WaitGroup
	//	for _, validatorRegistrationsSubmitter := range s.validatorRegistrationsSubmitters {
	//		if _, exists := signedRegistrations[validatorRegistrationsSubmitter.Address()]; !exists {
	//			log.Debug().Str("address", validatorRegistrationsSubmitter.Address()).Msg("No registrations for relay")
	//			continue
	//		}
	//
	//		wg.Add(1)
	//		go func(ctx context.Context, submitter builderclient.ValidatorRegistrationsSubmitter, signedRegistrations []*api.VersionedSignedValidatorRegistration) {
	//			defer wg.Done()
	//			if err := submitter.SubmitValidatorRegistrations(ctx, signedRegistrations); err != nil {
	//				log.Error().Err(err).Str("provider", submitter.Address()).Msg("Failed to submit validator registrations")
	//			}
	//		}(ctx, validatorRegistrationsSubmitter, signedRegistrations[validatorRegistrationsSubmitter.Address()])
	//	}
	wg.Wait()

	return nil
}
