package eval_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/jerry-jibu/kagent/orchestrator/internal/eval"
)

const (
	gateStrength      = "explicit"
	gatePolicyVersion = "d41d8cd98f00b204e9800998ecf8427e"
)

func TestParseDecision(t *testing.T) {
	t.Run("Given an evaluator response that is exactly a Decision JSON object, when parsed, assert that every field decodes into the Decision", func(t *testing.T) {
		raw := `{"outcome":"accept","strength":"explicit","policy_version":"` + gatePolicyVersion + `","reason":"authorized coding request"}`
		d, err := eval.ParseDecision(raw)
		if err != nil {
			t.Fatalf("parse failed: %v", err)
		}
		if d.Outcome != "accept" || d.Strength != gateStrength ||
			d.PolicyVersion != gatePolicyVersion || d.Reason != "authorized coding request" {
			t.Fatalf("decoded = %+v", d)
		}
	})

	t.Run("Given an evaluator response that is not a Decision JSON object, when parsed, assert that the rejection error is returned so the orchestrator never honors an unparseable claim", func(t *testing.T) {
		for name, raw := range map[string]string{
			"plain text":   "I happily accept this task!",
			"empty":        "",
			"wrong shape":  `{"result":"accept"}`,
			"json garbage": `{"outcome":`,
		} {
			t.Run("Given a response of "+name+", when parsed, assert that it is rejected", func(t *testing.T) {
				_, err := eval.ParseDecision(raw)
				if !errors.Is(err, eval.ErrReject) {
					t.Fatalf("err = %v, want ErrReject", err)
				}
			})
		}
	})
}

func TestDecisionValidate(t *testing.T) {
	t.Run("Given an accept decision that echoes the gate strength and policy version, when validated, assert that it is accepted", func(t *testing.T) {
		d := eval.Decision{Outcome: "accept", Strength: gateStrength, PolicyVersion: gatePolicyVersion}
		if err := d.Validate(gateStrength, gatePolicyVersion); err != nil {
			t.Fatalf("validate failed: %v", err)
		}
	})

	t.Run("Given an accept decision carrying a forged strength, when validated, assert that it is rejected", func(t *testing.T) {
		d := eval.Decision{Outcome: "accept", Strength: "inferred", PolicyVersion: gatePolicyVersion}
		err := d.Validate(gateStrength, gatePolicyVersion)
		if !errors.Is(err, eval.ErrReject) {
			t.Fatalf("err = %v, want ErrReject", err)
		}
		if !strings.Contains(err.Error(), "strength") {
			t.Fatalf("error %q must name the forged field", err)
		}
	})

	t.Run("Given an accept decision carrying a forged policy version, when validated, assert that it is rejected", func(t *testing.T) {
		d := eval.Decision{Outcome: "accept", Strength: gateStrength, PolicyVersion: "deadbeef"}
		err := d.Validate(gateStrength, gatePolicyVersion)
		if !errors.Is(err, eval.ErrReject) {
			t.Fatalf("err = %v, want ErrReject", err)
		}
		if !strings.Contains(err.Error(), "policy version") {
			t.Fatalf("error %q must name the forged field", err)
		}
	})

	t.Run("Given an accept decision missing the gate strength or the policy version, when validated, assert that it is rejected because accept must carry both", func(t *testing.T) {
		missingStrength := eval.Decision{Outcome: "accept", PolicyVersion: gatePolicyVersion}
		if err := missingStrength.Validate(gateStrength, gatePolicyVersion); !errors.Is(err, eval.ErrReject) {
			t.Fatalf("err = %v, want ErrReject for the missing strength", err)
		}
		missingVersion := eval.Decision{Outcome: "accept", Strength: gateStrength}
		if err := missingVersion.Validate(gateStrength, gatePolicyVersion); !errors.Is(err, eval.ErrReject) {
			t.Fatalf("err = %v, want ErrReject for the missing policy version", err)
		}
	})

	t.Run("Given a stop or ignore decision with empty echoes, when validated, assert that it is accepted; with forged echoes, assert that it is rejected", func(t *testing.T) {
		empty := eval.Decision{Outcome: "ignore", Reason: "insufficient evidence"}
		if err := empty.Validate(gateStrength, gatePolicyVersion); err != nil {
			t.Fatalf("empty echoes must validate, got %v", err)
		}

		forged := eval.Decision{Outcome: "stop", Strength: "explicit", PolicyVersion: "deadbeef"}
		if err := forged.Validate(gateStrength, gatePolicyVersion); !errors.Is(err, eval.ErrReject) {
			t.Fatalf("err = %v, want ErrReject for the forged policy version", err)
		}
	})

	t.Run("Given a decision whose outcome is outside the vocabulary, when validated, assert that it is rejected", func(t *testing.T) {
		d := eval.Decision{Outcome: "escalate"}
		if err := d.Validate(gateStrength, gatePolicyVersion); !errors.Is(err, eval.ErrReject) {
			t.Fatalf("err = %v, want ErrReject", err)
		}
	})
}
