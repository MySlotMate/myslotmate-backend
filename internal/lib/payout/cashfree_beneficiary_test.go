package payout

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeCashfree answers POST/GET /payout/beneficiary the way Cashfree's
// Beneficiary V2 API does (codes from its docs).
func fakeCashfree(t *testing.T, createStatus int, createBody string, getStatus int, getBody string) (*CashfreeProvider, *string) {
	t.Helper()
	var lastQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/payout/beneficiary" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			w.WriteHeader(createStatus)
			_, _ = w.Write([]byte(createBody))
			return
		}
		lastQuery = r.URL.RawQuery
		w.WriteHeader(getStatus)
		_, _ = w.Write([]byte(getBody))
	}))
	t.Cleanup(srv.Close)
	p := NewCashfreeProvider(CashfreeConfig{BaseURL: srv.URL, ClientID: "id", ClientSecret: "secret"}).(*CashfreeProvider)
	return p, &lastQuery
}

var bankReq = TransferRequest{
	BeneID:          "522cfab9af974620b23f5f2187b81f67",
	MethodType:      "bank",
	AccountNumber:   "1234567890506",
	IFSC:            "ubin0123456",
	BeneficiaryName: "Test Host",
}

func TestEnsureBeneficiary_Created(t *testing.T) {
	p, _ := fakeCashfree(t, 201, `{"beneficiary_id":"522cfab9af974620b23f5f2187b81f67","beneficiary_status":"VERIFIED"}`, 500, ``)
	id, err := p.ensureBeneficiary(context.Background(), bankReq)
	if err != nil || id != bankReq.BeneID {
		t.Fatalf("got %q, %v", id, err)
	}
}

func TestEnsureBeneficiary_SameIDAlreadyRegistered(t *testing.T) {
	p, _ := fakeCashfree(t, 409, `{"type":"invalid_request_error","code":"beneficiary_id_already_exists","message":"Beneficiary already exists with the given beneficiary_id"}`, 500, ``)
	id, err := p.ensureBeneficiary(context.Background(), bankReq)
	if err != nil || id != bankReq.BeneID {
		t.Fatalf("got %q, %v", id, err)
	}
}

// The live failure: the account was registered under an older id, so our id
// never existed and the transfer got beneficiary_not_found.
func TestEnsureBeneficiary_AccountRegisteredUnderOtherID(t *testing.T) {
	p, q := fakeCashfree(t,
		409, `{"type":"invalid_request_error","code":"beneficiary_already_exists","message":"Beneficiary already exists with the given bank_account_number and bank_ifsc"}`,
		200, `{"beneficiary_id":"oldmethod0000","beneficiary_status":"VERIFIED"}`)
	id, err := p.ensureBeneficiary(context.Background(), bankReq)
	if err != nil {
		t.Fatal(err)
	}
	if id != "oldmethod0000" {
		t.Fatalf("paid %q, want the id Cashfree already has", id)
	}
	if !strings.Contains(*q, "bank_account_number=1234567890506") || !strings.Contains(*q, "bank_ifsc=UBIN0123456") {
		t.Fatalf("lookup query %q", *q)
	}
}

func TestEnsureBeneficiary_ExistingBeneficiaryUnusable(t *testing.T) {
	p, _ := fakeCashfree(t,
		409, `{"code":"beneficiary_already_exists"}`,
		200, `{"beneficiary_id":"oldmethod0000","beneficiary_status":"INVALID"}`)
	if _, err := p.ensureBeneficiary(context.Background(), bankReq); err == nil || !strings.Contains(err.Error(), "INVALID") {
		t.Fatalf("want INVALID error, got %v", err)
	}
}

func TestEnsureBeneficiary_OtherErrorsFail(t *testing.T) {
	p, _ := fakeCashfree(t, 400, `{"code":"bank_ifsc_invalid","message":"bank_ifsc is invalid"}`, 500, ``)
	if _, err := p.ensureBeneficiary(context.Background(), bankReq); err == nil || !strings.Contains(err.Error(), "bank_ifsc_invalid") {
		t.Fatalf("want bank_ifsc_invalid error, got %v", err)
	}
}

// The code the live API actually returned on 2026-11-25 (TXN-29534).
func TestEnsureBeneficiary_LiveConflictCode(t *testing.T) {
	p, _ := fakeCashfree(t,
		409, `{"type":"invalid_request_error","code":"conflict_with_existing_beneficiary","message":"Beneficiary with the given bank_account_number and bank_ifsc already exists"}`,
		200, `{"beneficiary_id":"oldmethod0000","beneficiary_status":"VERIFIED"}`)
	id, err := p.ensureBeneficiary(context.Background(), bankReq)
	if err != nil || id != "oldmethod0000" {
		t.Fatalf("got %q, %v", id, err)
	}
}
