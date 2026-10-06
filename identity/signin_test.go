package identity_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nrynss/keel/identity"
	"github.com/nrynss/keel/sqlite"
	"github.com/nrynss/keel/wire"
)

// signInHandler wraps the code sign-in routes in the middleware, the way
// an app mounts them.
func (fx *fixture) signInHandler() http.Handler {
	return fx.svc.Middleware(fx.svc.CodeSignInHandler("/api/sign-in/code", "/api/sign-in/verify"))
}

// callCode posts one body to a sign-in route behind the middleware, with
// the cookie and the client address a test names.
func (fx *fixture) callCode(cookie *http.Cookie, route, body, remoteAddr string) *httptest.ResponseRecorder {
	fx.t.Helper()
	req := httptest.NewRequest(http.MethodPost, route, strings.NewReader(body))
	if cookie != nil {
		req.AddCookie(cookie)
	}
	req.RemoteAddr = remoteAddr
	rec := httptest.NewRecorder()
	fx.signInHandler().ServeHTTP(rec, req)
	return rec
}

// requestCode asks for one code and fails the test on any refusal.
func (fx *fixture) requestCode(cookie *http.Cookie, address string) {
	fx.t.Helper()
	rec := fx.callCode(cookie, "/api/sign-in/code", `{"address":`+quote(address)+`}`, "192.0.2.10:4000")
	if rec.Code != http.StatusAccepted {
		fx.t.Fatalf("code request for %s = %d, want 202: %s", address, rec.Code, rec.Body.String())
	}
}

// verifyCode posts one verify and returns the response.
func (fx *fixture) verifyCode(cookie *http.Cookie, address, code, choice string) *httptest.ResponseRecorder {
	fx.t.Helper()
	body := `{"address":` + quote(address) + `,"code":` + quote(code)
	if choice != "" {
		body += `,"choice":` + quote(choice)
	}
	body += `}`
	return fx.callCode(cookie, "/api/sign-in/verify", body, "192.0.2.10:4000")
}

// latestCode returns the code the last send carried.
func (fx *fixture) latestCode() string {
	fx.t.Helper()
	sent := fx.mail.sent()
	if len(sent) == 0 {
		fx.t.Fatal("no mail was sent")
	}
	return sent[len(sent)-1].code
}

// seedAccount signs one guest in by code, so the address is known.
func (fx *fixture) seedAccount(address string) (*http.Cookie, identity.User) {
	fx.t.Helper()
	cookie, user := fx.mint()
	fx.requestCode(cookie, address)
	rec := fx.verifyCode(cookie, address, fx.latestCode(), "")
	if rec.Code != http.StatusOK {
		fx.t.Fatalf("seed verify for %s = %d, want 200: %s", address, rec.Code, rec.Body.String())
	}
	return fx.cookie(rec), user
}

// quote renders one JSON string.
func quote(value string) string {
	raw, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

// addressHash computes the stored hash of one address, the way the
// service computes it with the test code key.
func addressHash(address string) string {
	mac := hmac.New(sha256.New, codeKey)
	_, _ = mac.Write([]byte(address))
	return hex.EncodeToString(mac.Sum(nil))
}

// TestCodeRequestAnswersTheSameForKnownAndUnknown checks the code route
// answers one body for an address that holds an account and one that
// never did, so the answer never names an account.
func TestCodeRequestAnswersTheSameForKnownAndUnknown(t *testing.T) {
	clock := &testClock{at: time.Unix(1758000000, 0)}
	fx := openFixture(t, clock)
	fx.seedAccount("known@example.com")
	cookie, _ := fx.mint()

	known := fx.callCode(cookie, "/api/sign-in/code", `{"address":"KNOWN@example.com"}`, "192.0.2.11:4000")
	unknown := fx.callCode(cookie, "/api/sign-in/code", `{"address":"unknown@example.com"}`, "192.0.2.12:4000")
	if known.Code != http.StatusAccepted || unknown.Code != http.StatusAccepted {
		t.Fatalf("codes = %d and %d, want 202 both", known.Code, unknown.Code)
	}
	if known.Body.String() != unknown.Body.String() {
		t.Fatalf("bodies differ, want one answer for known and unknown: %s vs %s",
			known.Body.String(), unknown.Body.String())
	}
	sent := fx.mail.sent()
	if len(sent) != 3 {
		t.Fatalf("%d mails sent, want one seed and one per request", len(sent))
	}
	if sent[1].address != "known@example.com" || sent[2].address != "unknown@example.com" {
		t.Fatalf("mails went to %q and %q, want the normalized addresses", sent[1].address, sent[2].address)
	}
}

// TestSendLimitRefusesSixthCodeToOneAddress checks the address ceiling
// refuses before the sixth send, and the window frees the address again.
func TestSendLimitRefusesSixthCodeToOneAddress(t *testing.T) {
	clock := &testClock{at: time.Unix(1758000000, 0)}
	fx := openFixture(t, clock)
	cookie, _ := fx.mint()
	for i := 0; i < 5; i++ {
		fx.requestCode(cookie, "capped@example.com")
	}
	rec := fx.callCode(cookie, "/api/sign-in/code", `{"address":"capped@example.com"}`, "192.0.2.13:4000")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("sixth code request = %d, want 429", rec.Code)
	}
	var refused struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &refused); err != nil {
		t.Fatalf("decode refusal: %v", err)
	}
	if refused.Error.Code != wire.CodeSendLimited {
		t.Fatalf("refusal code = %q, want %q", refused.Error.Code, wire.CodeSendLimited)
	}
	if wait := rec.Header().Get("Retry-After"); wait != "86400" {
		t.Fatalf("Retry-After = %q, want the full 24h window", wait)
	}
	if sent := len(fx.mail.sent()); sent != 5 {
		t.Fatalf("%d mails sent, want 5 with none on refusal", sent)
	}
	var rows int
	if err := fx.db.Reader().QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM sign_in_codes WHERE address_hash = ?", addressHash("capped@example.com")).Scan(&rows); err != nil {
		t.Fatalf("count code rows: %v", err)
	}
	if rows != 5 {
		t.Fatalf("%d code rows stored, want 5 with none on refusal", rows)
	}
	clock.advance(24*time.Hour + time.Second)
	fx.requestCode(cookie, "capped@example.com")
	if sent := len(fx.mail.sent()); sent != 6 {
		t.Fatalf("%d mails sent past the window, want 6", sent)
	}
}

// TestSendLimitRefusesEleventhRequestFromOneClient checks the client
// ceiling, and that a refusal consumes no client token.
func TestSendLimitRefusesEleventhRequestFromOneClient(t *testing.T) {
	clock := &testClock{at: time.Unix(1758000000, 0)}
	fx := openFixture(t, clock)
	cookie, _ := fx.mint()
	const client = "203.0.113.9:4000"
	for i := 0; i < 10; i++ {
		rec := fx.callCode(cookie, "/api/sign-in/code",
			`{"address":"client-`+fmt.Sprint(i)+`@example.com"}`, client)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("request %d = %d, want 202: %s", i+1, rec.Code, rec.Body.String())
		}
	}
	rec := fx.callCode(cookie, "/api/sign-in/code", `{"address":"client-10@example.com"}`, client)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("eleventh request = %d, want 429", rec.Code)
	}
	if wait := rec.Header().Get("Retry-After"); wait != "360" {
		t.Fatalf("Retry-After = %q, want the 6 minute refill", wait)
	}
	again := fx.callCode(cookie, "/api/sign-in/code", `{"address":"client-11@example.com"}`, client)
	if again.Code != http.StatusTooManyRequests || again.Header().Get("Retry-After") != "360" {
		t.Fatalf("second refusal = %d with Retry-After %q, want the same wait with no token spent",
			again.Code, again.Header().Get("Retry-After"))
	}
	if sent := len(fx.mail.sent()); sent != 10 {
		t.Fatalf("%d mails sent, want 10 with none on refusal", sent)
	}
	clock.advance(61 * time.Minute)
	rec = fx.callCode(cookie, "/api/sign-in/code", `{"address":"client-10@example.com"}`, client)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("request past the refill = %d, want 202", rec.Code)
	}
}

// TestCodeRowsKeepHashesOnly checks a stored code row holds HMAC hashes
// and never the plain address or the plain code.
func TestCodeRowsKeepHashesOnly(t *testing.T) {
	clock := &testClock{at: time.Unix(1758000000, 0)}
	fx := openFixture(t, clock)
	cookie, _ := fx.mint()
	const address = "private@example.com"
	fx.requestCode(cookie, address)
	code := fx.latestCode()

	// A fresh handle on the same file reads the row back, so the pin
	// never reads through the pools the service uses.
	fresh, err := sqlite.Open(t.Context(), sqlite.Config{Path: fx.path, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("reopen database: %v", err)
	}
	defer fresh.Close()
	var addressH, codeH, requesting string
	var used int64
	if err := fresh.Reader().QueryRowContext(t.Context(),
		"SELECT address_hash, code_hash, requesting_session, used_at FROM sign_in_codes ORDER BY created_at DESC LIMIT 1").
		Scan(&addressH, &codeH, &requesting, &used); err != nil {
		t.Fatalf("read code row: %v", err)
	}
	if addressH != addressHash(address) {
		t.Fatalf("address hash %q is not the HMAC of the address", addressH)
	}
	mac := hmac.New(sha256.New, codeKey)
	_, _ = mac.Write([]byte(code))
	if want := hex.EncodeToString(mac.Sum(nil)); codeH != want {
		t.Fatalf("code hash %q is not the HMAC of the code", codeH)
	}
	if requesting == "" {
		t.Fatal("code row names no requesting session")
	}
}

// TestVerifyAttachesOwnerOnFirstUse checks a fresh address attaches to
// the current guest, so the user id never changes and the kind flips.
func TestVerifyAttachesOwnerOnFirstUse(t *testing.T) {
	clock := &testClock{at: time.Unix(1758000000, 0)}
	fx := openFixture(t, clock)
	cookie, user := fx.mint()
	fx.requestCode(cookie, "fresh@example.com")
	oldSession := strings.SplitN(cookie.Value, ".", 2)[0]
	rec := fx.verifyCode(cookie, "fresh@example.com", fx.latestCode(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("first verify = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	rotated := fx.cookie(rec)
	if rotated == nil {
		t.Fatal("verify set no rotated cookie")
	}
	if strings.SplitN(rotated.Value, ".", 2)[0] == oldSession {
		t.Fatal("verify did not rotate the session")
	}
	var kind, holder string
	var revoked int
	if err := fx.db.Reader().QueryRowContext(t.Context(),
		"SELECT kind FROM users WHERE id = ?", user.ID).Scan(&kind); err != nil {
		t.Fatalf("read user: %v", err)
	}
	if kind != identity.KindOwner {
		t.Fatalf("kind = %q, want %q", kind, identity.KindOwner)
	}
	if err := fx.db.Reader().QueryRowContext(t.Context(),
		"SELECT user_id FROM identities WHERE provider = 'email' AND subject = ?",
		"fresh@example.com").Scan(&holder); err != nil {
		t.Fatalf("read identity: %v", err)
	}
	if holder != user.ID {
		t.Fatalf("identity holds %q, want the guest id %q", holder, user.ID)
	}
	if err := fx.db.Reader().QueryRowContext(t.Context(),
		"SELECT revoked FROM sessions WHERE id = ?", oldSession).Scan(&revoked); err != nil {
		t.Fatalf("read old session: %v", err)
	}
	if revoked != 1 {
		t.Fatal("verify left the old session live")
	}
}

// TestVerifyKeepsIdentityOnSameUser checks a second sign-in on the same
// address keeps one identity row and stays on the user.
func TestVerifyKeepsIdentityOnSameUser(t *testing.T) {
	clock := &testClock{at: time.Unix(1758000000, 0)}
	fx := openFixture(t, clock)
	cookie, user := fx.seedAccount("keeper@example.com")
	fx.requestCode(cookie, "keeper@example.com")
	rec := fx.verifyCode(cookie, "keeper@example.com", fx.latestCode(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("second sign-in = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var identities, users int
	if err := fx.db.Reader().QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM identities WHERE subject = ?", "keeper@example.com").Scan(&identities); err != nil {
		t.Fatalf("count identities: %v", err)
	}
	if err := fx.db.Reader().QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM users WHERE id = ?", user.ID).Scan(&users); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if identities != 1 || users != 1 {
		t.Fatalf("%d identities and %d users, want one of each", identities, users)
	}
}

// TestVerifySwitchesEmptyGuest checks a guest with no data moves to the
// account user, and the guest row stays for the app to decide on.
func TestVerifySwitchesEmptyGuest(t *testing.T) {
	clock := &testClock{at: time.Unix(1758000000, 0)}
	fx := openFixture(t, clock)
	fx.seedAccount("roamer@example.com")
	cookie, guest := fx.mint()
	oldSession := strings.SplitN(cookie.Value, ".", 2)[0]
	fx.requestCode(cookie, "roamer@example.com")
	rec := fx.verifyCode(cookie, "roamer@example.com", fx.latestCode(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("switch verify = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	rotated := fx.cookie(rec)
	sessionID := strings.SplitN(rotated.Value, ".", 2)[0]
	var holder string
	if err := fx.db.Reader().QueryRowContext(t.Context(),
		"SELECT user_id FROM sessions WHERE id = ?", sessionID).Scan(&holder); err != nil {
		t.Fatalf("read rotated session: %v", err)
	}
	if holder == guest.ID {
		t.Fatal("switched session points at the guest instead of the account")
	}
	var revoked int
	if err := fx.db.Reader().QueryRowContext(t.Context(),
		"SELECT revoked FROM sessions WHERE id = ?", oldSession).Scan(&revoked); err != nil {
		t.Fatalf("read old session: %v", err)
	}
	if revoked != 1 {
		t.Fatal("switch left the guest session live")
	}
	var identities int
	if err := fx.db.Reader().QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM identities").Scan(&identities); err != nil {
		t.Fatalf("count identities: %v", err)
	}
	if identities != 1 {
		t.Fatalf("%d identities, want the one attach", identities)
	}
	var guests int
	if err := fx.db.Reader().QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM users WHERE id = ?", guest.ID).Scan(&guests); err != nil {
		t.Fatalf("read guest row: %v", err)
	}
	if guests != 1 {
		t.Fatal("empty guest user row vanished on switch, want the app to own it")
	}
}

// TestVerifyConflictsOnGuestDataThenSwitches checks a guest with app
// data gets the conflict code with nothing changed, and the switch
// choice moves it while the same code stays live.
func TestVerifyConflictsOnGuestDataThenSwitches(t *testing.T) {
	clock := &testClock{at: time.Unix(1758000000, 0)}
	fx := openFixture(t, clock)
	fx.seedAccount("settled@example.com")
	cookie, guest := fx.mint()
	oldSession := strings.SplitN(cookie.Value, ".", 2)[0]
	fx.ownsData = true
	fx.requestCode(cookie, "settled@example.com")
	code := fx.latestCode()
	conflict := fx.verifyCode(cookie, "settled@example.com", code, "")
	if conflict.Code != http.StatusConflict {
		t.Fatalf("conflict verify = %d, want 409: %s", conflict.Code, conflict.Body.String())
	}
	var refused struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(conflict.Body.Bytes(), &refused); err != nil {
		t.Fatalf("decode conflict: %v", err)
	}
	if refused.Error.Code != wire.CodeGuestDataConflict {
		t.Fatalf("refusal code = %q, want %q", refused.Error.Code, wire.CodeGuestDataConflict)
	}
	var revoked int
	if err := fx.db.Reader().QueryRowContext(t.Context(),
		"SELECT revoked FROM sessions WHERE id = ?", oldSession).Scan(&revoked); err != nil {
		t.Fatalf("read old session: %v", err)
	}
	if revoked != 0 {
		t.Fatal("conflict revoked the guest session, want no change")
	}
	var used int64
	if err := fx.db.Reader().QueryRowContext(t.Context(),
		"SELECT used_at FROM sign_in_codes WHERE address_hash = ? AND requesting_session = ?",
		addressHash("settled@example.com"), oldSession).Scan(&used); err != nil {
		t.Fatalf("read code row: %v", err)
	}
	if used != 0 {
		t.Fatal("conflict consumed the code, want it live for the switch choice")
	}
	move := fx.verifyCode(cookie, "settled@example.com", code, "switch")
	if move.Code != http.StatusOK {
		t.Fatalf("switch verify = %d, want 200: %s", move.Code, move.Body.String())
	}
	rotated := fx.cookie(move)
	sessionID := strings.SplitN(rotated.Value, ".", 2)[0]
	var holder string
	if err := fx.db.Reader().QueryRowContext(t.Context(),
		"SELECT user_id FROM sessions WHERE id = ?", sessionID).Scan(&holder); err != nil {
		t.Fatalf("read rotated session: %v", err)
	}
	if holder == guest.ID {
		t.Fatal("switch choice left the device on the guest")
	}
}

// TestVerifyRefusesForeignSessionCode checks a code requested by one
// session never verifies on another, and stays live for its owner.
func TestVerifyRefusesForeignSessionCode(t *testing.T) {
	clock := &testClock{at: time.Unix(1758000000, 0)}
	fx := openFixture(t, clock)
	cookieA, _ := fx.mint()
	cookieB, _ := fx.mint()
	fx.requestCode(cookieA, "mine@example.com")
	code := fx.latestCode()
	if rec := fx.verifyCode(cookieB, "mine@example.com", code, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("foreign session verify = %d, want 401", rec.Code)
	}
	if rec := fx.verifyCode(cookieA, "mine@example.com", code, ""); rec.Code != http.StatusOK {
		t.Fatalf("owner verify = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

// TestVerifyClosesTheCodeAfterSixthWrongTry checks five wrong tries
// leave a correct code usable, and the sixth closes it.
func TestVerifyClosesTheCodeAfterSixthWrongTry(t *testing.T) {
	clock := &testClock{at: time.Unix(1758000000, 0)}
	fx := openFixture(t, clock)
	cookie, _ := fx.mint()
	fx.requestCode(cookie, "tries@example.com")
	code := fx.latestCode()
	for i := 0; i < 5; i++ {
		if rec := fx.verifyCode(cookie, "tries@example.com", "000000", ""); rec.Code != http.StatusUnauthorized {
			t.Fatalf("wrong try %d = %d, want 401", i+1, rec.Code)
		}
	}
	if rec := fx.verifyCode(cookie, "tries@example.com", code, ""); rec.Code != http.StatusOK {
		t.Fatalf("correct code after five wrong tries = %d, want 200", rec.Code)
	}
	cookie, _ = fx.mint()
	fx.requestCode(cookie, "closed@example.com")
	code = fx.latestCode()
	for i := 0; i < 6; i++ {
		if rec := fx.verifyCode(cookie, "closed@example.com", "000000", ""); rec.Code != http.StatusUnauthorized {
			t.Fatalf("wrong try %d = %d, want 401", i+1, rec.Code)
		}
	}
	if rec := fx.verifyCode(cookie, "closed@example.com", code, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("correct code after the sixth wrong try = %d, want 401", rec.Code)
	}
}

// TestVerifyRefusesExpiredCode checks a code past its ten minute life
// refuses, on the injected clock.
func TestVerifyRefusesExpiredCode(t *testing.T) {
	clock := &testClock{at: time.Unix(1758000000, 0)}
	fx := openFixture(t, clock)
	cookie, _ := fx.mint()
	fx.requestCode(cookie, "slow@example.com")
	code := fx.latestCode()
	clock.advance(10 * time.Minute)
	if rec := fx.verifyCode(cookie, "slow@example.com", code, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("expired verify = %d, want 401", rec.Code)
	}
}

// TestVerifyWorksOnce checks a consumed code never verifies again.
func TestVerifyWorksOnce(t *testing.T) {
	clock := &testClock{at: time.Unix(1758000000, 0)}
	fx := openFixture(t, clock)
	cookie, _ := fx.mint()
	fx.requestCode(cookie, "single@example.com")
	code := fx.latestCode()
	if rec := fx.verifyCode(cookie, "single@example.com", code, ""); rec.Code != http.StatusOK {
		t.Fatalf("first verify = %d, want 200", rec.Code)
	}
	rec := fx.verifyCode(cookie, "single@example.com", code, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("reused code = %d, want 401", rec.Code)
	}
}

// TestConcurrentFirstAttachSignsInBoth checks two devices signing one
// fresh address at once both land without a server fault. The loser
// joins the identity the winner attached instead of failing.
func TestConcurrentFirstAttachSignsInBoth(t *testing.T) {
	clock := &testClock{at: time.Unix(1758000000, 0)}
	fx := openFixture(t, clock)
	for round := 0; round < 8; round++ {
		address := fmt.Sprintf("roamer-%d@example.com", round)
		cookieA, _ := fx.mint()
		cookieB, _ := fx.mint()
		// Each device keeps its own client address, so the hourly client
		// ceiling the code route enforces never trips mid-test. The race
		// under test is the verify, not the send.
		if rec := fx.callCode(cookieA, "/api/sign-in/code", `{"address":`+quote(address)+`}`, "203.0.113.31:4000"); rec.Code != http.StatusAccepted {
			t.Fatalf("round %d device A code = %d, want 202: %s", round, rec.Code, rec.Body.String())
		}
		codeA := fx.latestCode()
		if rec := fx.callCode(cookieB, "/api/sign-in/code", `{"address":`+quote(address)+`}`, "203.0.113.32:4000"); rec.Code != http.StatusAccepted {
			t.Fatalf("round %d device B code = %d, want 202: %s", round, rec.Code, rec.Body.String())
		}
		codeB := fx.latestCode()
		cookies := []*http.Cookie{cookieA, cookieB}
		codes := []string{codeA, codeB}
		recs := make([]*httptest.ResponseRecorder, 2)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for device := 0; device < 2; device++ {
			wg.Add(1)
			go func(device int) {
				defer wg.Done()
				<-start
				recs[device] = fx.verifyCode(cookies[device], address, codes[device], "")
			}(device)
		}
		close(start)
		wg.Wait()
		users := make([]string, 2)
		for device := 0; device < 2; device++ {
			if recs[device] == nil {
				t.Fatalf("round %d device %d recorded no response", round, device)
			}
			if recs[device].Code != http.StatusOK {
				t.Fatalf("round %d device %d = %d, want 200: %s", round, device, recs[device].Code, recs[device].Body.String())
			}
			rotated := fx.cookie(recs[device])
			if rotated == nil {
				t.Fatalf("round %d device %d got no rotated cookie", round, device)
			}
			var holder string
			if err := fx.db.Reader().QueryRowContext(t.Context(),
				"SELECT user_id FROM sessions WHERE id = ?",
				strings.SplitN(rotated.Value, ".", 2)[0]).Scan(&holder); err != nil {
				t.Fatalf("round %d device %d: read session: %v", round, device, err)
			}
			users[device] = holder
		}
		if users[0] != users[1] {
			t.Fatalf("round %d devices landed on %q and %q, want one account", round, users[0], users[1])
		}
	}
	var identities int
	if err := fx.db.Reader().QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM identities").Scan(&identities); err != nil {
		t.Fatalf("count identities: %v", err)
	}
	if identities != 8 {
		t.Fatalf("%d identities, want one per round", identities)
	}
}

// TestSendFailureRemovesTheCode checks a refused send stores no dead
// code and spends no ceiling budget.
func TestSendFailureRemovesTheCode(t *testing.T) {
	clock := &testClock{at: time.Unix(1758000000, 0)}
	fx := openFixture(t, clock)
	cookie, _ := fx.mint()
	fx.mail.setFail(true)
	rec := fx.callCode(cookie, "/api/sign-in/code", `{"address":"outage@example.com"}`, "192.0.2.14:4000")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code request during an outage = %d, want 500", rec.Code)
	}
	var rows int
	if err := fx.db.Reader().QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM sign_in_codes WHERE address_hash = ?", addressHash("outage@example.com")).Scan(&rows); err != nil {
		t.Fatalf("count code rows: %v", err)
	}
	if rows != 0 {
		t.Fatalf("%d code rows survived a refused send, want none", rows)
	}
	fx.mail.setFail(false)
	for i := 0; i < 5; i++ {
		fx.requestCode(cookie, "outage@example.com")
	}
}
