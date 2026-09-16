package executor

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The OAuth CLI profile must declare per-message output_config when the caller
// requests it. These requests exercise header assembly without sending traffic.
// Upstream v7.2.158 intentionally forwards unmanaged caller betas (#5738).
// Assert that policy independently of beta order in the pinned CLI profile.
func TestApplyClaudeHeaders_PerTurnControlBeta(t *testing.T) {
	const perTurnBeta = "per-turn-control-2026-07-01"
	const unknownBeta = "totally-made-up-2030-01-01"
	for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens"} {
		for _, tc := range []struct {
			name      string
			requested string
			wantCount int
		}{
			{name: "requested", requested: perTurnBeta + "," + unknownBeta, wantCount: 1},
			{name: "duplicate_requested", requested: perTurnBeta + "," + perTurnBeta, wantCount: 1},
			{name: "not_requested", wantCount: 0},
			{name: "unknown_only", requested: unknownBeta, wantCount: 0},
		} {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				body := []byte(`{"model":"claude-fable-5-1","messages":[{"role":"user","content":"Reply with PONG."}]}`)
				if tc.wantCount == 1 {
					body = []byte(`{"model":"claude-fable-5-1","messages":[{"role":"user","content":"Reply with PONG."},{"role":"system","content":[],"output_config":{"effort":"low"}}]}`)
				}
				incoming := http.Header{}
				incoming.Set("Anthropic-Beta", "claude-code-20250219,mid-conversation-system-2026-04-07")
				if tc.requested != "" {
					incoming.Set("Anthropic-Beta", incoming.Get("Anthropic-Beta")+","+tc.requested)
				}
				req := httptest.NewRequest(http.MethodPost, "https://api.anthropic.com"+path, bytes.NewReader(body))
				if err := applyClaudeHeaders(req, claudeOAuthAuthForBetaPolicy(), claudeRaceProbeOAuthKey, false, nil, body, nil, incoming, false); err != nil {
					t.Fatalf("applyClaudeHeaders: %v", err)
				}
				got := req.Header.Get("Anthropic-Beta")
				count := 0
				unknownCount := 0
				for _, beta := range strings.Split(got, ",") {
					switch strings.TrimSpace(beta) {
					case perTurnBeta:
						count++
					case unknownBeta:
						unknownCount++
					}
				}
				if count != tc.wantCount {
					t.Errorf("per-turn-control beta count = %d, want %d; outgoing Anthropic-Beta = %q", count, tc.wantCount, got)
				}
				wantUnknown := 0
				if strings.Contains(tc.requested, unknownBeta) {
					wantUnknown = 1
				}
				if unknownCount != wantUnknown {
					t.Errorf("unmanaged beta count = %d, want %d", unknownCount, wantUnknown)
				}
				if path == "/v1/messages" && tc.wantCount == 1 && !strings.Contains(got, "mid-conversation-system-2026-04-07") {
					t.Errorf("per-turn directive lost required mid-conversation-system beta: %q", got)
				}
			})
		}
	}
}
