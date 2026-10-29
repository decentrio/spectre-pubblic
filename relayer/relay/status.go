package relay

import (
	"context"
	"sort"
	"time"

	"relayer/chain"
)

// statusProbeTimeout bounds the one RPC a status snapshot makes. A status
// command exists to answer during an incident, so it must answer even when the
// endpoint it asks about is the thing that is broken.
const statusProbeTimeout = 5 * time.Second

// Status is a point-in-time, read-only answer to "what is this relay path
// doing". Everything in it is either already in memory or bounded by
// statusProbeTimeout: nothing here may block on the pipeline it describes.
type Status struct {
	Path     string          `json:"path"`
	ClientID string          `json:"client_id"`
	Source   chain.ChainType `json:"source"`
	Dest     chain.ChainType `json:"destination"`

	// TrustedHeight is the destination client's height as this process last left
	// it. It is the in-memory value, so it can lag the chain when another relayer
	// or a manual update-client advanced the same client.
	TrustedHeight uint64 `json:"trusted_height"`
	// UpdateInFlight means the trusted height could not be read because a client
	// update is holding the lock. That is an answer rather than a gap: on the
	// Cosmos->EVM path the lock is held across proof generation, so seeing this is
	// seeing the proof.
	UpdateInFlight bool `json:"update_in_flight,omitempty"`

	// RelayableHeight is the highest source height provable right now. It is the
	// number that answers "why has nothing moved", and the only one here that
	// costs an RPC -- so it carries its own error instead of failing the snapshot.
	// ClientHeld is set while relaying is held because the destination client
	// is frozen or otherwise not active; it names the client and its status.
	ClientHeld string `json:"client_held,omitempty"`

	RelayableHeight    uint64 `json:"relayable_height"`
	RelayableHeightErr string `json:"relayable_height_error,omitempty"`

	// Loops is each background loop's last completed pass, stalest first. It is
	// the answer to "is this direction wedged or merely quiet", which otherwise
	// takes a goroutine dump that kills the process to find out.
	Loops []LoopHealth `json:"loops"`

	// Waiting is the set of packets parked BEFORE relay, oldest first. Paired with
	// the pending set from services, it is the "which half is stuck" split: this
	// one is "not relayable yet", that one is "relayed, awaiting delivery".
	Waiting []WaitingPacket `json:"waiting"`
}

// WaitingPacket is one parked packet with the age that separates "waiting for
// finality" from "never coming".
type WaitingPacket struct {
	Type     string        `json:"type"`
	Sequence uint64        `json:"sequence"`
	Height   uint64        `json:"height"`
	Age      time.Duration `json:"age_ns"`
	AgeText  string        `json:"age"`
}

// Status snapshots this path. Safe to call from any goroutine.
func (m *Module) Status(ctx context.Context) Status {
	s := Status{
		Path:     m.name,
		ClientID: m.clientID,
		Source:   m.src.Chain(),
		Dest:     m.dst.Chain(),
		Waiting:  m.waits.snapshot(),
		Loops:    m.beats.snapshot(),
	}

	// TryLock, not Lock. updateClientTo holds m.mu across the builder, which on
	// the Cosmos->EVM path means across proof generation -- and a status command
	// that blocks for the length of a Groth16 proof is useless at exactly the
	// moment someone runs it.
	if held := m.clientHold.Load(); held != nil {
		s.ClientHeld = *held
	}

	if m.mu.TryLock() {
		s.TrustedHeight = m.lastHeight
		m.mu.Unlock()
	} else {
		s.UpdateInFlight = true
	}

	probeCtx, cancel := context.WithTimeout(ctx, statusProbeTimeout)
	defer cancel()
	if height, err := m.src.RelayableHeight(probeCtx); err != nil {
		s.RelayableHeightErr = err.Error()
	} else {
		s.RelayableHeight = height
	}
	return s
}

// snapshot lists what is parked, oldest first -- the order an operator reads in,
// because the oldest entry is the one most likely to be stuck.
func (w *waitTracker) snapshot() []WaitingPacket {
	w.mu.Lock()
	now := w.now()
	out := make([]WaitingPacket, 0, len(w.entries))
	for key, entry := range w.entries {
		age := now.Sub(entry.since)
		out = append(out, WaitingPacket{
			Type:     key.typ.String(),
			Sequence: key.seq,
			Height:   entry.height,
			Age:      age,
			AgeText:  age.Round(time.Second).String(),
		})
	}
	w.mu.Unlock()

	sort.Slice(out, func(i, j int) bool {
		if out[i].Age != out[j].Age {
			return out[i].Age > out[j].Age
		}
		return out[i].Sequence < out[j].Sequence // stable output for equal ages
	})
	return out
}

// LoopHealth is one background loop's last completed pass.
type LoopHealth struct {
	Worker    string        `json:"worker"`
	LastTick  time.Time     `json:"last_tick"`
	Since     time.Duration `json:"since_ns"`
	SinceText string        `json:"since"`
	Note      string        `json:"note,omitempty"`
}

// sortLoopsByStalest puts the longest-silent loop first: it is the one an
// operator is looking for, and a truncated list must not hide it.
func sortLoopsByStalest(loops []LoopHealth) {
	sort.Slice(loops, func(i, j int) bool {
		if loops[i].Since != loops[j].Since {
			return loops[i].Since > loops[j].Since
		}
		return loops[i].Worker < loops[j].Worker
	})
}
