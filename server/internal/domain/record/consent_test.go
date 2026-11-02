package record

import (
	"testing"
	"time"
)

func TestValidateConsent(t *testing.T) {
	valid := Consent{OwnerID: "u1234567", Purpose: PurposeFullRecord, Enabled: false, Version: 0}
	if err := ValidateConsent(valid); err != nil {
		t.Fatalf("baseline consent: %v", err)
	}
	valid.Enabled = true
	valid.Version = 1
	valid.UpdatedAt = time.Now()
	if err := ValidateConsent(valid); err != nil {
		t.Fatalf("enabled consent: %v", err)
	}
	valid.Purpose = ""
	if err := ValidateConsent(valid); err == nil {
		t.Fatal("empty purpose was accepted")
	}
}

func TestValidateQuota(t *testing.T) {
	value := Quota{OwnerID: "u1234567", RetentionDays: DefaultRetentionDays, LimitBytes: DefaultQuotaBytes,
		Version: 1, UpdatedAt: time.Now()}
	if err := ValidateQuota(value); err != nil {
		t.Fatalf("default quota: %v", err)
	}
	value.ReservedBytes = -1
	if err := ValidateQuota(value); err == nil {
		t.Fatal("negative reservation was accepted")
	}
}
