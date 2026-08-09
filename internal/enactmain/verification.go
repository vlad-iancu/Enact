package enactmain

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	restful "github.com/emicklei/go-restful/v3"

	"enact/internal/logging"
	"enact/internal/requesthelper"
	"enact/internal/users"
)

// newVerificationToken mints a verification token and its storage hash.
// Only the hash is persisted; the plaintext token exists solely inside the
// emailed link.
func newVerificationToken() (token, hash string, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", err
	}
	token = hex.EncodeToString(raw)
	sum := sha256.Sum256([]byte(token))
	return token, hex.EncodeToString(sum[:]), nil
}

// verificationLink builds the emailed URL pointing at this service.
func (a *MainAPI) verificationLink(email, token string) string {
	return fmt.Sprintf("%s/auth/verify?email=%s&token=%s",
		a.publicBaseURL, url.QueryEscape(email), url.QueryEscape(token))
}

// sendVerificationEmail stamps a fresh token onto the user record and mails
// the verification link. The caller decides how a send failure surfaces.
func (a *MainAPI) sendVerificationEmail(req *restful.Request, logger *logging.Logger, user users.User) error {
	token, hash, err := newVerificationToken()
	if err != nil {
		return fmt.Errorf("mint verification token: %w", err)
	}
	user.VerificationTokenHash = hash
	user.VerificationExpiresAt = time.Now().UTC().Add(a.verificationTTL)
	user.UpdatedAt = time.Now().UTC()
	if err := a.users.Save(req.Request.Context(), user); err != nil {
		return fmt.Errorf("save verification token: %w", err)
	}
	logger.Info("verification token stored", "expires_at", user.VerificationExpiresAt)

	link := a.verificationLink(user.Email, token)
	body := fmt.Sprintf(
		"Hello %s,\n\nverify your enact account by opening this link:\n\n%s\n\nThe link expires in %s. If you did not create this account, ignore this email.\n",
		user.DisplayName, link, a.verificationTTL)
	if err := a.mailer.Send(req.Request.Context(), user.Email, "Verify your enact account", body); err != nil {
		return err
	}
	logger.Info("verification email sent")
	return nil
}

// verifyEmail is the emailed link's target: it validates the token and marks
// the account verified, then sends the browser to the login page.
func (a *MainAPI) verifyEmail(req *restful.Request, resp *restful.Response) {
	email := req.QueryParameter("email")
	logger := requesthelper.Logger(req, a.logger).WithFields("email", email)
	logger.Info("email verification requested")

	redirect := func(ok bool, reason string) {
		target := a.postLoginTarget()
		if a.frontendURL == "" {
			target = "/login"
		}
		sep := "?"
		if ok {
			target += sep + "verified=1"
		} else {
			target += sep + "verified=0&reason=" + url.QueryEscape(reason)
		}
		http.Redirect(resp.ResponseWriter, req.Request, target, http.StatusFound)
	}

	token := req.QueryParameter("token")
	if email == "" || token == "" {
		logger.Warn("verification link missing parameters")
		redirect(false, "invalid link")
		return
	}
	user, found, err := a.users.GetByEmail(req.Request.Context(), email)
	if err != nil {
		logger.Error("failed to load user for verification", "err", err)
		redirect(false, "try again later")
		return
	}
	if !found || user.VerificationTokenHash == "" {
		logger.Warn("verification for unknown user or no pending token", "found", found)
		redirect(false, "invalid link")
		return
	}
	if time.Now().After(user.VerificationExpiresAt) {
		logger.Warn("verification token expired", "expired_at", user.VerificationExpiresAt)
		redirect(false, "link expired")
		return
	}
	sum := sha256.Sum256([]byte(token))
	if subtle.ConstantTimeCompare([]byte(hex.EncodeToString(sum[:])), []byte(user.VerificationTokenHash)) != 1 {
		logger.Warn("verification token mismatch")
		redirect(false, "invalid link")
		return
	}

	user.EmailVerified = true
	user.VerificationTokenHash = ""
	user.VerificationExpiresAt = time.Time{}
	user.UpdatedAt = time.Now().UTC()
	if err := a.users.Save(req.Request.Context(), user); err != nil {
		logger.Error("failed to persist verification", "err", err)
		redirect(false, "try again later")
		return
	}
	logger.Info("email verified", "user_id", user.ID)
	redirect(true, "")
}

// resendVerification re-issues the verification email. It always returns
// 204 so responses do not reveal which emails have accounts.
func (a *MainAPI) resendVerification(req *restful.Request, resp *restful.Response) {
	logger := requesthelper.Logger(req, a.logger)
	logger.Info("verification resend requested")

	var body struct {
		Email string `json:"email"`
	}
	dec := json.NewDecoder(req.Request.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		logger.Warn("invalid resend body", "err", err)
		requesthelper.WriteError(req, resp, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
		return
	}
	logger = logger.WithFields("email", users.NormalizeEmail(body.Email))

	user, found, err := a.users.GetByEmail(req.Request.Context(), body.Email)
	if err == nil && found && !user.EmailVerified {
		if sendErr := a.sendVerificationEmail(req, logger, user); sendErr != nil {
			logger.Error("failed to resend verification email", "err", sendErr)
		}
	} else {
		logger.Info("resend skipped", "found", found, "err", err)
	}
	resp.WriteHeader(http.StatusNoContent)
}
