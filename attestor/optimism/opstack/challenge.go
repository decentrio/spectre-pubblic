package opstack

import (
	"context"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"go.uber.org/zap"
)

// ChallengeHook receives mismatch verdicts: proposals whose root claim the
// attestor's honest replay contradicts. The P3 implementation submits the
// on-chain challenge / misbehaviour; P1 only raises the alarm.
type ChallengeHook interface {
	OnMismatch(ctx context.Context, game ProposedRoot, localRoot [32]byte) error
}

// LogChallengeHook is the P1 placeholder: log at error level and stop. It
// never fails, so a mismatch verdict is always recorded exactly once.
type LogChallengeHook struct {
	Logger *zap.SugaredLogger
}

func (h *LogChallengeHook) OnMismatch(_ context.Context, game ProposedRoot, localRoot [32]byte) error {
	h.Logger.Errorw("[opstack attestor] OUTPUT ROOT MISMATCH — proposed root contradicts honest derivation",
		"game_index", game.GameIndex,
		"game_address", game.GameAddress.Hex(),
		"l2_block", game.L2BlockNumber,
		"claimed_root", hexutil.Encode(game.RootClaim[:]),
		"derived_root", hexutil.Encode(localRoot[:]),
	)
	return nil
}
