package service

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"myslotmate-backend/internal/lib/payment"
	"myslotmate-backend/internal/models"
	"myslotmate-backend/internal/repository"

	"github.com/google/uuid"
	_ "github.com/lib/pq"
)

// Paid-budget Boost against a real Postgres with the migrations applied.
// Skipped unless TEST_DATABASE_URL is set. Every fixture uses fresh ids, so
// it can run on a shared scratch database.

type fakeRazorpay struct{ links int }

func (f *fakeRazorpay) CreateOrder(_ context.Context, r payment.OrderRequest) (*payment.OrderResponse, error) {
	return &payment.OrderResponse{OrderID: "order_" + uuid.NewString()[:12], AmountCents: r.AmountCents, Currency: "INR"}, nil
}
func (f *fakeRazorpay) VerifyPaymentSignature(r payment.VerifyRequest) bool {
	return r.Signature == "good"
}
func (f *fakeRazorpay) GetKeyID() string { return "rzp_test" }
func (f *fakeRazorpay) CreatePaymentLink(_ context.Context, r payment.PaymentLinkRequest) (*payment.PaymentLinkResponse, error) {
	f.links++
	return &payment.PaymentLinkResponse{ID: "plink_" + uuid.NewString()[:12], ShortURL: "https://rzp.io/x", Status: "created"}, nil
}

type boostFixture struct {
	db      *sql.DB
	svc     *BoostService
	pay     *fakeRazorpay
	userID  uuid.UUID
	hostID  uuid.UUID
	eventID uuid.UUID
}

func boostDB(t *testing.T) *boostFixture {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	db, err := sql.Open("postgres", url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	f := &boostFixture{db: db, pay: &fakeRazorpay{}}
	f.userID, f.hostID, f.eventID = f.newHost(t, true)
	billing := NewBillingService(repository.NewBillingRepository(db), nil).WithDB(db)
	f.svc = NewBoostService(db, repository.NewEventRepository(db), repository.NewEventPriceTierRepository(db),
		repository.NewHostRepository(db), repository.NewUserRepository(db), nil, nil, "", "").
		WithPlan(f.pay, nil, billing)
	return f
}

// newHost adds an approved host (Premium when premium) with one live ₹800 event.
func (f *boostFixture) newHost(t *testing.T, premium bool) (uuid.UUID, uuid.UUID, uuid.UUID) {
	t.Helper()
	u, h, e := uuid.New(), uuid.New(), uuid.New()
	tag := u.String()[:8]
	mustExec(t, f.db, `INSERT INTO users (id, auth_uid, email) VALUES ($1, $2, $3)`, u, "uid-"+tag, tag+"@test.local")
	mustExec(t, f.db, `INSERT INTO hosts (id, user_id, slug, first_name, last_name, application_status) VALUES ($1, $2, $3, 'Test', 'Host', 'approved')`, h, u, "host-"+tag)
	mustExec(t, f.db, `INSERT INTO events (id, host_id, title, slug, time, status, price_cents, cancellation_policy)
		VALUES ($1, $2, 'Jam '||$3::text, 'jam-'||$3::text, now() + interval '30 days', 'live', 80000, 'moderate')`, e, h, tag)
	if premium {
		mustExec(t, f.db, `INSERT INTO premium_memberships (host_id, status, granted) VALUES ($1, 'active', true)`, h)
	}
	return u, h, e
}

func mustExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// acceptedBudgetBoost stores a budget Boost, proposed at 8% and accepted:
// ₹1,000 a day for 7 days = ₹7,000 + ₹1,260 GST.
func (f *boostFixture) acceptedBudgetBoost(t *testing.T, eventID, hostID uuid.UUID, due time.Time) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	r := repository.NewBoostRepository(f.db)
	daily := int64(100000)
	from, until := istDay(time.Now().AddDate(0, 0, 1)), istDay(time.Now().AddDate(0, 0, 7))
	b := &models.Boost{ID: uuid.New(), EventID: eventID, HostID: hostID, Status: models.BoostUnderReview, Channels: []string{"instagram"},
		RequestedAt: time.Now(), PricingModel: models.BoostPricingBudget, BudgetStatus: models.BudgetUnpaid,
		DailyBudgetCents: &daily, BudgetFrom: &from, BudgetUntil: &until, BudgetTotalCents: 700000, BudgetGSTCents: 126000,
		Rates: []models.BoostRate{{TicketKey: "", TicketName: "Ticket", PriceCents: 80000, GuideRateBps: 800, RateBps: 800, BandVersion: 2}}}
	if err := r.Create(ctx, b); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := r.SetProposal(ctx, b.ID, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := r.SetAccepted(ctx, b.ID, "127.0.0.1", "test", models.BoostBudgetTermsVersion, &due); err != nil {
		t.Fatal(err)
	}
	return b.ID
}

func boostCode(err error) string {
	var be *BoostError
	if errors.As(err, &be) {
		return be.Code
	}
	if err != nil {
		return "other: " + err.Error()
	}
	return ""
}

func TestDBBoostBands(t *testing.T) {
	f := boostDB(t)
	r := repository.NewBoostRepository(f.db)
	for model, want := range map[string]int{models.BoostPricingNoUpfront: 1, models.BoostPricingBudget: 2} {
		bands, err := r.Bands(context.Background(), model)
		if err != nil || len(bands) != 5 || bands[0].Version != want {
			t.Fatalf("%s: %d bands, version %v, err %v (want version %d)", model, len(bands), bands, err, want)
		}
	}
}

func TestDBBoostBudgetFlow(t *testing.T) {
	f := boostDB(t)
	ctx := context.Background()
	id := f.acceptedBudgetBoost(t, f.eventID, f.hostID, time.Now().Add(7*24*time.Hour))

	// Checkout order: the server prices it.
	order, err := f.svc.CreatePayment(ctx, f.userID, id, PaymentRequest{Kind: "budget"})
	if err != nil || order.AmountCents != 826000 {
		t.Fatalf("order = %+v, %v; want ₹8,260", order, err)
	}
	// Bad signature, then a wrong amount from the webhook: both refused.
	if _, err := f.svc.VerifyPayment(ctx, f.userID, id, order.PaymentID, VerifyRequest{OrderID: order.OrderID, PaymentID: "pay_x", Signature: "bad"}); boostCode(err) != "invalid_signature" {
		t.Fatalf("bad signature: %v", err)
	}
	if _, err := f.svc.HandleGatewayPayment(ctx, order.OrderID, "", "pay_x", 100, nil); err == nil {
		t.Fatal("a wrong amount was accepted")
	}
	pay1, pay2 := "pay_"+uuid.NewString()[:12], "pay_"+uuid.NewString()[:12]
	// Webhook, then a replayed webhook, then verify: paid once.
	for i := 0; i < 2; i++ {
		handled, err := f.svc.HandleGatewayPayment(ctx, order.OrderID, "", pay1, 826000, nil)
		if !handled || err != nil {
			t.Fatalf("webhook %d: handled=%v err=%v", i, handled, err)
		}
	}
	b, err := f.svc.VerifyPayment(ctx, f.userID, id, order.PaymentID, VerifyRequest{OrderID: order.OrderID, PaymentID: pay1, Signature: "good"})
	if err != nil || b.BudgetStatus != models.BudgetPaid || b.PaidCents != 700000 {
		t.Fatalf("after verify: %+v, %v", b, err)
	}
	var n int
	_ = f.db.QueryRow(`SELECT COUNT(*) FROM boost_payments WHERE boost_id = $1 AND status = 'paid'`, id).Scan(&n)
	if n != 1 {
		t.Fatalf("%d paid payments, want 1", n)
	}
	// A wallet order isn't ours.
	if handled, err := f.svc.HandleGatewayPayment(ctx, "order_wallet", "", "pay_w", 500, nil); handled || err != nil {
		t.Fatalf("wallet order: handled=%v err=%v", handled, err)
	}
	// A second budget payment is refused.
	if _, err := f.svc.CreatePayment(ctx, f.userID, id, PaymentRequest{Kind: "budget"}); boostCode(err) != "invalid_state" && boostCode(err) != "already_paid" {
		t.Fatalf("second budget payment: %v", err)
	}

	// Go-live needs a creative in use.
	if _, err := f.svc.GoLive(ctx, "admin", id, nil); boostCode(err) != "no_creative" {
		t.Fatalf("go-live without creative: %v", err)
	}
	r := repository.NewBoostRepository(f.db)
	cr := &models.BoostCreative{ID: uuid.New(), BoostID: id, Type: "post", Files: []models.StoredFile{{Key: "boosts/x/creatives/y/a.png", Name: "a.png", MimeType: "image/png", Size: 10}},
		Channels: []string{}, UploadedByKind: "host", UploadedBy: f.hostID.String(), Status: models.CreativeInUse, CreatedAt: time.Now()}
	if err := r.CreateCreative(ctx, cr); err != nil {
		t.Fatal(err)
	}
	b, err = f.svc.GoLive(ctx, "admin", id, nil)
	if err != nil || b.Status != models.BoostLive || b.LiveUntil == nil {
		t.Fatalf("go-live: %+v, %v", b, err)
	}
	if want := budgetEnd(b); !b.LiveUntil.Equal(*want) {
		t.Fatalf("live_until %v, want %v", b.LiveUntil, want)
	}

	// Spend is capped at the paid ₹7,000.
	today := istDay(time.Now())
	if _, err := f.svc.AdminSpend(ctx, "admin", id, []SpendInput{{Day: today, Channel: "instagram", SpendCents: 500000, Reach: 900, Clicks: 30}}); err != nil {
		t.Fatalf("spend: %v", err)
	}
	if _, err := f.svc.AdminSpend(ctx, "admin", id, []SpendInput{{Day: today, Channel: "google", SpendCents: 300000}}); boostCode(err) != "over_budget" {
		t.Fatalf("over budget: %v", err)
	}
	b, _ = f.svc.repo.GetByID(ctx, id)
	if b.SpentCents != 500000 || b.Reach != 900 {
		t.Fatalf("refused spend leaked: spent %d reach %d", b.SpentCents, b.Reach)
	}
	// Editing the same day/channel replaces it.
	if b, err = f.svc.AdminSpend(ctx, "admin", id, []SpendInput{{Day: today, Channel: "instagram", SpendCents: 600000}}); err != nil || b.SpentCents != 600000 {
		t.Fatalf("upsert: %+v %v", b, err)
	}

	// Top-up by payment link (a higher daily budget), paid via payment_link.paid.
	link, err := f.svc.AdminPaymentLink(ctx, "admin", id, PaymentRequest{Kind: "top_up", DailyBudgetCents: ptrI64(150000)})
	if err != nil || link.Payment.AmountCents <= 0 || f.pay.links != 1 {
		t.Fatalf("payment link: %+v %v", link, err)
	}
	if handled, err := f.svc.HandleGatewayPayment(ctx, "order_"+uuid.NewString()[:12], *link.Payment.RazorpayLinkID, pay2, link.Payment.TotalCents(), nil); !handled || err != nil {
		t.Fatalf("link paid: %v %v", handled, err)
	}
	b, _ = f.svc.repo.GetByID(ctx, id)
	if *b.DailyBudgetCents != 150000 || b.BudgetTotalCents != 700000+link.Payment.AmountCents || b.PaidCents != 700000+link.Payment.AmountCents {
		t.Fatalf("top-up not applied: daily %d total %d paid %d", *b.DailyBudgetCents, b.BudgetTotalCents, b.PaidCents)
	}

	// Results read the log and the bookings.
	mustExec(t, f.db, `INSERT INTO bookings (event_id, user_id, quantity, occurrence_date, boost_id, status) VALUES ($1, $2, 2, now(), $3, 'confirmed')`, f.eventID, f.userID, id)
	res, err := f.svc.Results(ctx, f.userID, id)
	if err != nil || res.Totals.SpendCents != 600000 || res.Totals.TicketsSold != 2 {
		t.Fatalf("results: %+v %v", res, err)
	}

	// Close: only ended, only with a final report; then spend locks.
	if _, err := f.svc.AdminClose(ctx, "admin", id); boostCode(err) != "invalid_state" {
		t.Fatalf("close while live: %v", err)
	}
	if _, err := f.svc.EndWithNote(ctx, "admin", "admin", id, time.Now(), "budget_spent", "Budget used up"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.AdminClose(ctx, "admin", id); boostCode(err) != "no_final_report" {
		t.Fatalf("close without report: %v", err)
	}
	rep := &models.BoostReport{ID: uuid.New(), BoostID: id, Kind: "final", Title: "Final", File: models.StoredFile{Key: "boosts/x/reports/y/f.pdf", Name: "f.pdf", MimeType: "application/pdf", Size: 5}, UploadedBy: "admin", CreatedAt: time.Now()}
	if err := r.CreateReport(ctx, rep); err != nil {
		t.Fatal(err)
	}
	if b, err = f.svc.AdminClose(ctx, "admin", id); err != nil || b.ClosedAt == nil || b.ReportsCount != 1 {
		t.Fatalf("close: %+v %v", b, err)
	}
	if _, err := f.svc.AdminSpend(ctx, "admin", id, []SpendInput{{Day: today, Channel: "instagram", SpendCents: 1}}); boostCode(err) != "closed" {
		t.Fatalf("spend after close: %v", err)
	}
}

func TestDBBoostPaymentExpiry(t *testing.T) {
	f := boostDB(t)
	ctx := context.Background()
	id := f.acceptedBudgetBoost(t, f.eventID, f.hostID, time.Now().Add(-time.Minute))
	order, err := f.svc.CreatePayment(ctx, f.userID, id, PaymentRequest{Kind: "budget"})
	if boostCode(err) != "payment_expired" {
		t.Fatalf("pay after due: %+v %v", order, err)
	}
	f.svc.expireUnpaid(ctx)
	b, _ := f.svc.repo.GetByID(ctx, id)
	if b.Status != models.BoostCancelled || b.EndedReason == nil || *b.EndedReason != "payment_expired" {
		t.Fatalf("not expired: %s %v", b.Status, b.EndedReason)
	}
}

func TestDBBoostOfflineAndReverse(t *testing.T) {
	f := boostDB(t)
	ctx := context.Background()
	id := f.acceptedBudgetBoost(t, f.eventID, f.hostID, time.Now().Add(24*time.Hour))
	in := OfflinePayment{PaymentRequest: PaymentRequest{Kind: "budget"}, AmountCents: 100, PaidOn: istDay(time.Now()), Reference: "UTR1"}
	if _, err := f.svc.AdminOfflinePayment(ctx, "admin", id, in); boostCode(err) != "amount_mismatch" {
		t.Fatalf("wrong amount: %v", err)
	}
	in.AmountCents = 826000
	b, err := f.svc.AdminOfflinePayment(ctx, "admin", id, in)
	if err != nil || b.BudgetStatus != models.BudgetPaid {
		t.Fatalf("offline: %+v %v", b, err)
	}
	ps, _ := f.svc.repo.ListPayments(ctx, id)
	b, err = f.svc.AdminReversePayment(ctx, "admin", id, ps[0].ID, "never arrived")
	if err != nil || b.BudgetStatus != models.BudgetUnpaid || b.PaidCents != 0 {
		t.Fatalf("reverse: %+v %v", b, err)
	}
}

func TestDBBudgetInvoice(t *testing.T) {
	f := boostDB(t)
	ctx := context.Background()
	mustExec(t, f.db, `INSERT INTO billing_settings (id, enabled, legal_name, gstin, address, state_code, sac_code, budget_sac_code)
		VALUES (true, true, 'MySlotMate Test', '19ABCDE1234F1Z5', 'Kolkata', '19', '998599', '998365')
		ON CONFLICT (id) DO UPDATE SET enabled = true, legal_name = EXCLUDED.legal_name, gstin = EXCLUDED.gstin,
		  address = EXCLUDED.address, state_code = EXCLUDED.state_code, sac_code = EXCLUDED.sac_code, budget_sac_code = EXCLUDED.budget_sac_code`)
	t.Cleanup(func() { f.db.Exec(`UPDATE billing_settings SET enabled = false, budget_sac_code = ''`) })
	id := f.acceptedBudgetBoost(t, f.eventID, f.hostID, time.Now().Add(24*time.Hour))
	in := OfflinePayment{PaymentRequest: PaymentRequest{Kind: "budget"}, AmountCents: 826000, PaidOn: istDay(time.Now()), Reference: "UTR2"}
	if _, err := f.svc.AdminOfflinePayment(ctx, "admin", id, in); err != nil {
		t.Fatal(err)
	}
	f.svc.invoicePending(ctx) // a second try must not issue a second invoice
	docs, err := f.svc.Documents(ctx, f.userID, id)
	if err != nil || len(docs) != 1 || docs[0].AmountCents != 700000 || docs[0].GSTCents != 126000 || docs[0].Kind != "invoice" {
		t.Fatalf("docs = %+v, %v", docs, err)
	}
	ps, _ := f.svc.repo.ListPayments(ctx, id)
	if _, err := f.svc.AdminReversePayment(ctx, "admin", id, ps[0].ID, "bounced"); err != nil {
		t.Fatal(err)
	}
	if docs, _ = f.svc.Documents(ctx, f.userID, id); len(docs) != 2 || docs[1].Kind != "credit_note" {
		t.Fatalf("credit note missing: %+v", docs)
	}
}

func TestDBPremium(t *testing.T) {
	f := boostDB(t)
	ctx := context.Background()
	userID, hostID, eventID := f.newHost(t, false)

	// Not Premium: no Boost.
	if err := f.svc.requirePremium(ctx, f.svc.repo, hostID); boostCode(err) != "premium_required" {
		t.Fatalf("gate: %v", err)
	}
	if m, err := f.svc.HostPremium(ctx, userID); err != nil || m.Status != "none" {
		t.Fatalf("none: %+v %v", m, err)
	}
	if m, err := f.svc.ApplyPremium(ctx, userID, "please"); err != nil || m.Status != "applied" {
		t.Fatalf("apply: %+v %v", m, err)
	}
	if _, err := f.svc.ApplyPremium(ctx, userID, ""); boostCode(err) != "already_applied" {
		t.Fatalf("apply twice: %v", err)
	}
	if _, err := f.svc.PremiumDecline(ctx, "admin", hostID, PremiumDecision{Reason: "not yet"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.ApplyPremium(ctx, userID, ""); boostCode(err) != "too_soon" {
		t.Fatalf("reapply within 30 days: %v", err)
	}
	m, err := f.svc.PremiumGrant(ctx, "admin", hostID, PremiumDecision{ManagerName: "Riya"})
	if err != nil || m.Status != "active" || !m.Granted || m.ManagerName == nil {
		t.Fatalf("grant: %+v %v", m, err)
	}
	if !f.svc.IsPremium(ctx, hostID) {
		t.Fatal("badge off for an active host")
	}
	// Removed: locked again.
	if _, err := f.svc.PremiumRemove(ctx, "admin", hostID, PremiumDecision{Reason: "x"}); err != nil {
		t.Fatal(err)
	}
	if f.svc.IsPremium(ctx, hostID) {
		t.Fatal("removed host still Premium")
	}
	d, err := f.svc.PremiumGet(ctx, hostID)
	if err != nil || len(d.Log) != 4 {
		t.Fatalf("history: %+v %v", d, err)
	}
	_ = eventID
}

func TestDBRemindersAndLists(t *testing.T) {
	f := boostDB(t)
	ctx := context.Background()
	r := f.svc.repo
	id := f.acceptedBudgetBoost(t, f.eventID, f.hostID, time.Now().Add(5*24*time.Hour))
	mustExec(t, f.db, `UPDATE event_boosts SET accepted_at = now() - interval '3 days' WHERE id = $1`, id)
	c, err := r.ReminderCandidates(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, x := range c.Unpaid {
		found = found || x == id
	}
	if !found {
		t.Fatal("unpaid boost not a reminder candidate")
	}
	f.svc.sendReminders(ctx, time.Now())
	if ok, _ := r.LogOnce(ctx, id, "reminder_payment_2d"); ok {
		t.Fatal("reminder would be sent twice")
	}
	list, err := r.ListByStatus(ctx, "", "unpaid", 500)
	if err != nil || len(list) == 0 {
		t.Fatalf("budget_status filter: %d %v", len(list), err)
	}
	if _, err := r.AllBands(ctx); err != nil {
		t.Fatal(err)
	}
	v, err := f.svc.PublishBands(ctx, "admin", []models.BoostBand{{MinPriceCents: 0, GuideRateBps: 500, MinRateBps: 400, MaxRateBps: 800}})
	if err != nil || v < 3 {
		t.Fatalf("publish: %d %v", v, err)
	}
	t.Cleanup(func() { f.db.Exec(`DELETE FROM boost_rate_bands WHERE version = $1`, v) })
	st, err := f.svc.Settings(ctx)
	if err != nil || st.CreativeVideoMaxMB != 200 {
		t.Fatalf("settings: %+v %v", st, err)
	}
	if _, err := f.svc.SaveSettings(ctx, *st); err != nil {
		t.Fatal(err)
	}
}

func ptrI64(v int64) *int64 { return &v }

// Every remaining repository query runs (no SQL or type errors).
func TestDBRepoSmoke(t *testing.T) {
	f := boostDB(t)
	ctx := context.Background()
	r := f.svc.repo
	id := f.acceptedBudgetBoost(t, f.eventID, f.hostID, time.Now().Add(24*time.Hour))
	cr := &models.BoostCreative{ID: uuid.New(), BoostID: id, Type: "reel", Files: []models.StoredFile{{Key: "k", Name: "a.mp4", MimeType: "video/mp4", Size: 1}},
		Channels: []string{"instagram"}, UploadedByKind: "admin", UploadedBy: "admin", Status: models.CreativeAwaitingApproval, CreatedAt: time.Now()}
	check := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	check("create creative", r.CreateCreative(ctx, cr))
	c, err := f.svc.ApproveCreative(ctx, f.userID, id, cr.ID)
	check("approve", err)
	if c.Status != models.CreativeApproved || len(c.Files) != 1 {
		t.Fatalf("approved creative = %+v", c)
	}
	_, err = f.svc.AdminCreativeStatus(ctx, "admin", id, cr.ID, models.CreativeInUse, "")
	check("in use", err)
	list, err := r.ListCreatives(ctx, id)
	check("list creatives", err)
	if len(list) != 1 || list[0].Status != models.CreativeInUse {
		t.Fatalf("creatives = %+v", list)
	}
	host := &models.BoostCreative{ID: uuid.New(), BoostID: id, Type: "brand", Files: []models.StoredFile{}, Channels: []string{},
		UploadedByKind: "host", UploadedBy: f.hostID.String(), Status: models.CreativeSubmitted, CreatedAt: time.Now()}
	check("create host creative", r.CreateCreative(ctx, host))
	check("delete", f.svc.DeleteCreative(ctx, f.userID, id, host.ID))

	rep := &models.BoostReport{ID: uuid.New(), BoostID: id, Kind: "update", Title: "Week 1", File: models.StoredFile{Key: "k", Name: "r.csv", MimeType: "text/csv", Size: 1}, UploadedBy: "admin", CreatedAt: time.Now()}
	check("create report", r.CreateReport(ctx, rep))
	old, err := r.GetReportForUpdate(ctx, id, rep.ID)
	check("get report", err)
	check("replace report", r.MarkReportReplaced(ctx, old.ID))
	all, err := r.ListReports(ctx, id, true)
	check("reports history", err)
	cur, err := r.ListReports(ctx, id, false)
	check("reports", err)
	if len(all) != 1 || len(cur) != 0 {
		t.Fatalf("history %d current %d", len(all), len(cur))
	}

	check("channels", r.SetChannels(ctx, id, []string{"google", "whatsapp"}))
	check("pricing", r.SetPricingModel(ctx, id, models.BoostPricingBudget, models.BudgetUnpaid))
	check("top-up days", r.ApplyTopUp(ctx, id, 200000, 36000, 2, nil))
	b, err := r.GetByID(ctx, id)
	check("get", err)
	if *b.BudgetUntil != istDay(time.Now().AddDate(0, 0, 9)) || b.BudgetTotalCents != 900000 || len(b.Channels) != 2 {
		t.Fatalf("after top-up: until %s total %d channels %v", *b.BudgetUntil, b.BudgetTotalCents, b.Channels)
	}
	_, err = r.SpendEntries(ctx, id)
	check("spend entries", err)
	_, err = r.UninvoicedPayments(ctx, 10)
	check("uninvoiced", err)
	_, err = r.DueForPayment(ctx, time.Now())
	check("due", err)
	_, err = r.BudgetInvoices(ctx, id)
	check("budget invoices", err)
	_, _, err = f.svc.PremiumList(ctx, "")
	check("premium list", err)
	_, err = r.PaymentIDByGateway(ctx, "", "plink_none")
	check("by link", err)
	_, err = r.TicketsPerDay(ctx, id)
	check("tickets per day", err)
}

func TestDBHostActivity(t *testing.T) {
	f := boostDB(t)
	ctx := context.Background()
	id := f.acceptedBudgetBoost(t, f.eventID, f.hostID, time.Now().Add(24*time.Hour))
	r := f.svc.repo
	for _, a := range []struct{ kind, action, details string }{
		{"admin", "proposed", `{}`}, {"host", "accepted", `{}`}, {"admin", "declined", `{"reason":"no"}`},
	} {
		if err := r.Log(ctx, id, a.kind, "x", a.action, map[string]any{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.PremiumLog(ctx, f.hostID, "admin", "x", "grant", nil); err != nil {
		t.Fatal(err)
	}
	items, err := f.svc.HostActivity(ctx, f.userID)
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	for _, it := range items {
		actions = append(actions, it.Action)
	}
	got := strings.Join(actions, ",")
	if got != "grant,declined,proposed" {
		t.Fatalf("actions = %s (the host's own accept must not show)", got)
	}
}

// fakeFiles serves every key as its own name, for the ZIP test.
type fakeFiles struct{}

func (fakeFiles) PresignBoostUpload(context.Context, string, string, int64) (string, error) {
	return "", nil
}
func (fakeFiles) SignBoostDownload(k, _ string) string   { return "view:" + k }
func (fakeFiles) SignBoostAttachment(k, _ string) string { return "save:" + k }
func (fakeFiles) OpenBoostObject(_ context.Context, k string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("bytes of " + k)), nil
}
func (fakeFiles) HeadBoostObject(context.Context, string) (int64, string, error) {
	return 1, "image/png", nil
}

func TestDBCreativesZip(t *testing.T) {
	f := boostDB(t)
	ctx := context.Background()
	f.svc.files = fakeFiles{}
	id := f.acceptedBudgetBoost(t, f.eventID, f.hostID, time.Now().Add(24*time.Hour))
	mk := func(typ string, at time.Time, names ...string) uuid.UUID {
		c := &models.BoostCreative{ID: uuid.New(), BoostID: id, Type: typ, Channels: []string{}, UploadedByKind: "host",
			UploadedBy: "h", Status: models.CreativeSubmitted, CreatedAt: at}
		for _, n := range names {
			c.Files = append(c.Files, models.StoredFile{Key: "boosts/" + id.String() + "/creatives/x/" + n, Name: n, MimeType: "image/png", Size: 1})
		}
		if err := f.svc.repo.CreateCreative(ctx, c); err != nil {
			t.Fatal(err)
		}
		return c.ID
	}
	now := time.Now()
	mk("post", now.Add(-time.Hour), "a.png", "b.png")
	story := mk("story", now, "c.png")

	read := func(only *uuid.UUID) []string {
		var buf bytes.Buffer
		if err := f.svc.AdminCreativesZip(ctx, id, only, &buf); err != nil {
			t.Fatal(err)
		}
		zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, e := range zr.File {
			names = append(names, e.Name)
		}
		return names
	}
	if got := strings.Join(read(nil), ","); got != "01-post-host/01-a.png,01-post-host/02-b.png,02-story-host/01-c.png" {
		t.Fatalf("all = %s", got)
	}
	if got := strings.Join(read(&story), ","); got != "02-story-host/01-c.png" {
		t.Fatalf("one = %s", got)
	}
	list, _ := f.svc.AdminCreatives(ctx, id)
	if list[0].Files[0].DownloadURL == "" || !strings.HasPrefix(list[0].Files[0].DownloadURL, "save:") {
		t.Fatalf("download_url = %q", list[0].Files[0].DownloadURL)
	}
}
