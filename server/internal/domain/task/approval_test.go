package task

import "testing"

func validApprovalCost() *ApprovalCost {
	return &ApprovalCost{AmountMicros: 1200, Currency: "USD", Basis: ApprovalCostBasisEstimate}
}

func evidenceCompletePendingRequest() PendingRequest {
	return PendingRequest{
		EvidencePointer:      "task_step:s1234567",
		CurrentCost:          validApprovalCost(),
		TotalCost:            &ApprovalCost{AmountMicros: 5400, Currency: "USD", Basis: ApprovalCostBasisSettled},
		TriggerRule:          "sensitive_host_tool",
		RejectionConsequence: "If rejected, this exact tool call is not executed.",
	}
}

func TestApprovalCostValidate(t *testing.T) {
	if err := (*ApprovalCost)(nil).Validate(); err != nil {
		t.Fatalf("nil approval cost should validate (unknown is a legitimate state): %v", err)
	}
	if err := validApprovalCost().Validate(); err != nil {
		t.Fatalf("valid approval cost should validate: %v", err)
	}
	cases := []struct {
		name string
		cost *ApprovalCost
	}{
		{name: "negative amount", cost: &ApprovalCost{AmountMicros: -1, Currency: "USD", Basis: ApprovalCostBasisEstimate}},
		{name: "lowercase currency", cost: &ApprovalCost{AmountMicros: 1, Currency: "usd", Basis: ApprovalCostBasisEstimate}},
		{name: "short currency", cost: &ApprovalCost{AmountMicros: 1, Currency: "US", Basis: ApprovalCostBasisEstimate}},
		{name: "missing currency", cost: &ApprovalCost{AmountMicros: 1, Basis: ApprovalCostBasisEstimate}},
		{name: "unknown basis", cost: &ApprovalCost{AmountMicros: 1, Currency: "USD", Basis: "actual"}},
		{name: "missing basis", cost: &ApprovalCost{AmountMicros: 1, Currency: "USD"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.cost.Validate(); err == nil {
				t.Fatal("invalid approval cost must fail validation")
			}
		})
	}
}

func TestPendingRequestMissingEvidenceFields(t *testing.T) {
	complete := evidenceCompletePendingRequest()
	if got := complete.MissingEvidenceFields(); len(got) != 0 {
		t.Fatalf("evidence-complete pending request must miss nothing, got %v", got)
	}
	if !complete.EvidenceComplete() {
		t.Fatal("evidence-complete pending request must report complete")
	}
	// Nil costs are unknown, not missing: a request with no trusted cost
	// number is still complete.
	unknownCosts := evidenceCompletePendingRequest()
	unknownCosts.CurrentCost, unknownCosts.TotalCost = nil, nil
	if got := unknownCosts.MissingEvidenceFields(); len(got) != 0 {
		t.Fatalf("pending request with unknown costs must miss nothing, got %v", got)
	}
	cases := []struct {
		name   string
		mutate func(*PendingRequest)
		want   []string
	}{
		{name: "no evidence pointer", mutate: func(p *PendingRequest) { p.EvidencePointer = "" }, want: []string{"evidence_pointer"}},
		{name: "no trigger rule", mutate: func(p *PendingRequest) { p.TriggerRule = "" }, want: []string{"trigger_rule"}},
		{name: "no rejection consequence", mutate: func(p *PendingRequest) { p.RejectionConsequence = "" }, want: []string{"rejection_consequence"}},
		{name: "negative current cost", mutate: func(p *PendingRequest) {
			p.CurrentCost = &ApprovalCost{AmountMicros: -5, Currency: "USD", Basis: ApprovalCostBasisEstimate}
		}, want: []string{"current_cost"}},
		{name: "invalid total cost basis", mutate: func(p *PendingRequest) {
			p.TotalCost = &ApprovalCost{AmountMicros: 5, Currency: "USD", Basis: "guess"}
		}, want: []string{"total_cost"}},
		{name: "legacy row misses all three text fields", mutate: func(p *PendingRequest) {
			p.EvidencePointer, p.TriggerRule, p.RejectionConsequence = "", "", ""
			p.CurrentCost, p.TotalCost = nil, nil
		}, want: []string{"evidence_pointer", "trigger_rule", "rejection_consequence"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := evidenceCompletePendingRequest()
			tc.mutate(&request)
			got := request.MissingEvidenceFields()
			if len(got) != len(tc.want) {
				t.Fatalf("missing evidence fields = %v, want %v", got, tc.want)
			}
			for i, field := range tc.want {
				if got[i] != field {
					t.Fatalf("missing evidence fields = %v, want %v", got, tc.want)
				}
			}
			if request.EvidenceComplete() {
				t.Fatal("pending request missing evidence must not report complete")
			}
		})
	}
}
