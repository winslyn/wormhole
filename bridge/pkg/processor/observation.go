package processor

import (
	"context"
	"encoding/hex"
	"fmt"
	bridge_common "github.com/certusone/wormhole/bridge/pkg/common"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"go.uber.org/zap"

	gossipv1 "github.com/certusone/wormhole/bridge/pkg/proto/gossip/v1"
	"github.com/certusone/wormhole/bridge/pkg/vaa"
)

var (
	observationsReceivedTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "wormhole_observations_received_total",
			Help: "Total number of raw VAA observations received from gossip",
		})
	observationsReceivedByGuardianAddressTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "wormhole_observations_signed_by_guardian_total",
			Help: "Total number of signed and verified VAA observations grouped by guardian address",
		}, []string{"addr"})
	observationsFailedTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "wormhole_observations_verification_failures_total",
			Help: "Total number of observations verification failure, grouped by failure reason",
		}, []string{"cause"})
	observationsUnknownTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "wormhole_observations_unknown_total",
			Help: "Total number of verified observations we haven't seen ourselves",
		})
	observationsDirectSubmissionsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "wormhole_observations_direct_submissions_queued_total",
			Help: "Total number of observations for a specific target chain that were queued for direct submission",
		}, []string{"target_chain"})
	observationsDirectSubmissionSuccessTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "wormhole_observations_direct_submission_success_total",
			Help: "Total number of observations for a specific target chain that succeeded",
		}, []string{"target_chain"})
)

// handleObservation processes a remote VAA observation, verifies it, checks whether the VAA has met quorum,
// and assembles and submits a valid VAA if possible.
func (p *Processor) handleObservation(ctx context.Context, m *gossipv1.SignedObservation) {
	// SECURITY: at this point, observations received from the p2p network are fully untrusted (all fields!)
	//
	// Note that observations are never tied to the (verified) p2p identity key - the p2p network
	// identity is completely decoupled from the guardian identity, p2p is just transport.

	hash := hex.EncodeToString(m.Hash)

	p.logger.Info("received observation",
		zap.String("digest", hash),
		zap.String("signature", hex.EncodeToString(m.Signature)),
		zap.String("addr", hex.EncodeToString(m.Addr)))

	observationsReceivedTotal.Inc()

	// Verify the Guardian's signature. This verifies that m.Signature matches m.Hash and recovers
	// the public key that was used to sign the payload.
	pk, err := crypto.Ecrecover(m.Hash, m.Signature)
	if err != nil {
		p.logger.Warn("failed to verify signature on observation",
			zap.String("digest", hash),
			zap.String("signature", hex.EncodeToString(m.Signature)),
			zap.String("addr", hex.EncodeToString(m.Addr)),
			zap.Error(err))
		observationsFailedTotal.WithLabelValues("invalid_signature").Inc()
		return
	}

	// Verify that m.Addr matches the public key that signed m.Hash.
	their_addr := common.BytesToAddress(m.Addr)
	signer_pk := common.BytesToAddress(crypto.Keccak256(pk[1:])[12:])

	if their_addr != signer_pk {
		p.logger.Info("invalid observation - address does not match pubkey",
			zap.String("digest", hash),
			zap.String("signature", hex.EncodeToString(m.Signature)),
			zap.String("addr", hex.EncodeToString(m.Addr)),
			zap.String("pk", signer_pk.Hex()))
		observationsFailedTotal.WithLabelValues("pubkey_mismatch").Inc()
		return
	}

	// Determine which guardian set to use. The following cases are possible:
	//
	//  - We have already seen the message and generated ourVAA. In this case, use the guardian set valid at the time,
	//    even if the guardian set was updated. Old guardian sets remain valid for longer than aggregation state,
	//    and the guardians in the old set stay online and observe and sign messages for the transition period.
	//
	//  - We have not yet seen the message. In this case, we assume the latest guardian set because that's what
	//    we will store once we do see the message.
	//
	// This ensures that during a guardian set update, a node which observed a given message with either the old
	// or the new guardian set can achieve consensus, since both the old and the new set would achieve consensus,
	// assuming that 2/3+ of the old and the new guardian set have seen the message and will periodically attempt
	// to retransmit their observations such that nodes who initially dropped the signature will get a 2nd chance.
	//
	// During an update, vaaState.signatures can contain signatures from *both* guardian sets.
	//
	var gs *bridge_common.GuardianSet
	if p.state.vaaSignatures[hash] != nil && p.state.vaaSignatures[hash].gs != nil {
		gs = p.state.vaaSignatures[hash].gs
	} else {
		gs = p.gs
	}

	// We haven't yet observed the trusted guardian set on Ethereum, and therefore, it's impossible to verify it.
	// May as well not have received it/been offline - drop it and wait for the guardian set.
	if gs == nil {
		p.logger.Warn("dropping observations since we haven't initialized our guardian set yet",
			zap.String("digest", their_addr.Hex()),
			zap.String("their_addr", their_addr.Hex()),
		)
		observationsFailedTotal.WithLabelValues("uninitialized_guardian_set").Inc()
		return
	}

	// Verify that m.Addr is included in the guardian set. If it's not, drop the message. In case it's us
	// who have the outdated guardian set, we'll just wait for the message to be retransmitted eventually.
	_, ok := gs.KeyIndex(their_addr)
	if !ok {
		p.logger.Warn("received observation by unknown guardian - is our guardian set outdated?",
			zap.String("digest", their_addr.Hex()),
			zap.String("their_addr", their_addr.Hex()),
			zap.Uint32("index", gs.Index),
			zap.Any("keys", gs.KeysAsHexStrings()),
		)
		observationsFailedTotal.WithLabelValues("unknown_guardian").Inc()
		return
	}

	// Hooray! Now, we have verified all fields on SignedObservation and know that it includes
	// a valid signature by an active guardian. We still don't fully trust them, as they may be
	// byzantine, but now we know who we're dealing with.

	// We can now count events by guardian without worry about cardinality explosions:
	observationsReceivedByGuardianAddressTotal.WithLabelValues(their_addr.Hex()).Inc()

	// []byte isn't hashable in a map. Paying a small extra cost for encoding for easier debugging.
	if p.state.vaaSignatures[hash] == nil {
		// We haven't yet seen this event ourselves, and therefore do not know what the VAA looks like.
		// However, we have established that a valid guardian has signed it, and therefore we can
		// already start aggregating signatures for it.
		//
		// A malicious guardian can potentially DoS this by creating fake observations at a faster rate than they decay,
		// leading to a slow out-of-memory crash. We do not attempt to automatically mitigate spam attacks with valid
		// signatures - such byzantine behavior would be plainly visible and would be dealt with by kicking them.

		observationsUnknownTotal.Inc()

		p.state.vaaSignatures[hash] = &vaaState{
			firstObserved: time.Now(),
			signatures:    map[common.Address][]byte{},
			source:        "unknown",
		}
	}

	p.state.vaaSignatures[hash].signatures[their_addr] = m.Signature

	// Aggregate all valid signatures into a list of vaa.Signature and construct signed VAA.
	agg := make([]bool, len(gs.Keys))
	var sigs []*vaa.Signature
	for i, a := range gs.Keys {
		s, ok := p.state.vaaSignatures[hash].signatures[a]

		if ok {
			var bs [65]byte
			if n := copy(bs[:], s); n != 65 {
				panic(fmt.Sprintf("invalid sig len: %d", n))
			}

			sigs = append(sigs, &vaa.Signature{
				Index:     uint8(i),
				Signature: bs,
			})
		}

		agg[i] = ok
	}

	if p.state.vaaSignatures[hash].ourVAA != nil {
		// We have seen it on chain!
		// Deep copy the VAA and add signatures
		v := p.state.vaaSignatures[hash].ourVAA
		signed := &vaa.VAA{
			Version:          v.Version,
			GuardianSetIndex: v.GuardianSetIndex,
			Signatures:       sigs,
			Timestamp:        v.Timestamp,
			Nonce:            v.Nonce,
			Sequence:         v.Sequence,
			EmitterChain:     v.EmitterChain,
			EmitterAddress:   v.EmitterAddress,
			Payload:          v.Payload,
			ConsistencyLevel: v.ConsistencyLevel,
		}

		// 2/3+ majority required for VAA to be valid - wait until we have quorum to submit VAA.
		quorum := CalculateQuorum(len(gs.Keys))

		p.logger.Info("aggregation state for VAA",
			zap.String("digest", hash),
			zap.Any("set", gs.KeysAsHexStrings()),
			zap.Uint32("index", gs.Index),
			zap.Bools("aggregation", agg),
			zap.Int("required_sigs", quorum),
			zap.Int("have_sigs", len(sigs)),
		)

		if len(sigs) >= quorum && !p.state.vaaSignatures[hash].submitted {
			vaaBytes, err := signed.Marshal()
			if err != nil {
				panic(err)
			}

			// Store signed VAA in database.
			p.logger.Info("signed VAA with quorum",
				zap.String("digest", hash),
				zap.Any("vaa", signed),
				zap.String("bytes", hex.EncodeToString(vaaBytes)),
				zap.String("message_id", signed.MessageID()))

			if err := p.db.StoreSignedVAA(signed); err != nil {
				p.logger.Error("failed to store signed VAA", zap.Error(err))
			}
			p.state.vaaSignatures[hash].submitted = true
		} else {
			p.logger.Info("quorum not met or already submitted, doing nothing",
				zap.String("digest", hash))
		}
	} else {
		p.logger.Info("we have not yet seen this VAA - temporarily storing signature",
			zap.String("digest", hash),
			zap.Bools("aggregation", agg))

	}
}
