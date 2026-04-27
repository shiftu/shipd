package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	qrcode "github.com/skip2/go-qrcode"
)

// WeixinAccount is what's persisted to disk after a successful QR login.
// The bot_token is the long-lived bearer credential the long-poll loop and
// the sendmessage call both need; baseurl can shift to a redirect host
// during the scan handshake.
type WeixinAccount struct {
	AccountID string    `json:"account_id"`
	Token     string    `json:"token"`
	BaseURL   string    `json:"base_url"`
	UserID    string    `json:"user_id"`
	SavedAt   time.Time `json:"saved_at"`
}

// WeixinQRLoginConfig customizes the login flow. Defaults are sensible for
// an interactive terminal session.
type WeixinQRLoginConfig struct {
	BotType  string        // iLink classifies bots; "3" matches the Hermes default
	Timeout  time.Duration // wall-clock budget across refreshes; default 8 minutes
	StateDir string        // where to persist the credentials
	Out      io.Writer     // where to print prompts + the QR code; default os.Stdout
	BaseURL  string        // override iLinkBaseURL for tests
}

func (c *WeixinQRLoginConfig) defaults() {
	if c.BotType == "" {
		c.BotType = "3"
	}
	if c.Timeout == 0 {
		c.Timeout = 8 * time.Minute
	}
	if c.Out == nil {
		c.Out = os.Stdout
	}
	if c.BaseURL == "" {
		c.BaseURL = iLinkBaseURL
	}
}

// QRLoginWeixin walks the user through the iLink QR login: fetches a fresh
// QR code, prints both the scan URL and an ASCII QR to the terminal, polls
// for status until "confirmed", and persists the resulting bot_token to
// <state_dir>/<account_id>.json.
//
// Returns the persisted account on success. Most users should call this from
// `shipd gateway weixin-login` once and never directly.
func QRLoginWeixin(ctx context.Context, cfg WeixinQRLoginConfig) (*WeixinAccount, error) {
	cfg.defaults()
	deadline := time.Now().Add(cfg.Timeout)
	cli := newILinkClient(cfg.BaseURL, "")

	qrValue, qrURL, err := fetchBotQR(ctx, cli, cfg.BotType)
	if err != nil {
		return nil, fmt.Errorf("fetch QR: %w", err)
	}
	printQR(cfg.Out, qrValue, qrURL)

	const refreshLimit = 3
	refreshes := 0
	curBaseURL := cfg.BaseURL

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		statusCli := newILinkClient(curBaseURL, "")
		var status weixinQRStatusResp
		if err := statusCli.get(ctx, fmt.Sprintf("%s?qrcode=%s", iLinkEPQRStatus, qrValue), &status); err != nil {
			// Transient errors (timeouts, redirects we haven't applied yet)
			// shouldn't kill the flow; back off briefly and retry.
			time.Sleep(time.Second)
			continue
		}

		switch status.Status {
		case "wait":
			fmt.Fprint(cfg.Out, ".")
			time.Sleep(time.Second)
		case "scaned":
			fmt.Fprintln(cfg.Out, "\n已扫码，请在微信里确认...")
			time.Sleep(time.Second)
		case "scaned_but_redirect":
			if status.RedirectHost != "" {
				curBaseURL = "https://" + status.RedirectHost
			}
			time.Sleep(time.Second)
		case "expired":
			refreshes++
			if refreshes > refreshLimit {
				return nil, errors.New("QR code expired too many times; re-run login")
			}
			fmt.Fprintf(cfg.Out, "\n二维码已过期，正在刷新 (%d/%d)...\n", refreshes, refreshLimit)
			qrValue, qrURL, err = fetchBotQR(ctx, cli, cfg.BotType)
			if err != nil {
				return nil, fmt.Errorf("refresh QR: %w", err)
			}
			printQR(cfg.Out, qrValue, qrURL)
		case "confirmed":
			if status.IlinkBotID == "" || status.BotToken == "" {
				return nil, errors.New("login confirmed but credential payload was incomplete")
			}
			acc := &WeixinAccount{
				AccountID: status.IlinkBotID,
				Token:     status.BotToken,
				BaseURL:   firstNonEmptyStr(status.BaseURL, cfg.BaseURL),
				UserID:    status.IlinkUserID,
				SavedAt:   time.Now().UTC(),
			}
			if err := SaveWeixinAccount(cfg.StateDir, acc); err != nil {
				return nil, fmt.Errorf("save credentials: %w", err)
			}
			fmt.Fprintf(cfg.Out, "\n微信连接成功 account=%s\n", acc.AccountID)
			return acc, nil
		default:
			// Unknown status: log and keep polling.
			fmt.Fprintf(cfg.Out, "\n(unknown status: %s)\n", status.Status)
			time.Sleep(time.Second)
		}
	}
	return nil, fmt.Errorf("QR login timed out after %s", cfg.Timeout)
}

type weixinQRResp struct {
	iLinkResp
	QRCode           string `json:"qrcode"`
	QRCodeImgContent string `json:"qrcode_img_content"`
}

type weixinQRStatusResp struct {
	iLinkResp
	Status       string `json:"status"`
	RedirectHost string `json:"redirect_host"`
	IlinkBotID   string `json:"ilink_bot_id"`
	BotToken     string `json:"bot_token"`
	BaseURL      string `json:"baseurl"`
	IlinkUserID  string `json:"ilink_user_id"`
}

func fetchBotQR(ctx context.Context, cli *iLinkClient, botType string) (string, string, error) {
	var resp weixinQRResp
	if err := cli.get(ctx, fmt.Sprintf("%s?bot_type=%s", iLinkEPGetBotQR, botType), &resp); err != nil {
		return "", "", err
	}
	if resp.QRCode == "" {
		return "", "", errors.New("QR response missing qrcode field")
	}
	return resp.QRCode, resp.QRCodeImgContent, nil
}

// printQR renders the QR to the terminal — both as the raw scan URL (so the
// user can paste it into a phone's browser as a fallback) and as ASCII art.
// Uses go-qrcode's compact encoding which is half the height of the default.
func printQR(w io.Writer, qrValue, qrURL string) {
	scanData := qrURL
	if scanData == "" {
		scanData = qrValue
	}
	fmt.Fprintln(w, "\n请使用微信扫描以下二维码：")
	if qrURL != "" {
		fmt.Fprintln(w, qrURL)
	}
	q, err := qrcode.New(scanData, qrcode.Medium)
	if err == nil {
		fmt.Fprintln(w, q.ToSmallString(false))
	}
}

// --- persistence ---

// SaveWeixinAccount writes the credentials to <stateDir>/<account_id>.json
// with 0600 permissions.
func SaveWeixinAccount(stateDir string, acc *WeixinAccount) error {
	if stateDir == "" {
		return errors.New("state dir not set")
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(stateDir, acc.AccountID+".json")
	body, err := json.MarshalIndent(acc, "", "  ")
	if err != nil {
		return err
	}
	return writeFile0600(path, body)
}

// LoadWeixinAccount reads the credentials previously saved by SaveWeixinAccount.
func LoadWeixinAccount(stateDir, accountID string) (*WeixinAccount, error) {
	path := filepath.Join(stateDir, accountID+".json")
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var acc WeixinAccount
	if err := json.Unmarshal(body, &acc); err != nil {
		return nil, err
	}
	return &acc, nil
}

// loadSyncBuf returns the persisted long-poll cursor for an account, or "".
func loadSyncBuf(stateDir, accountID string) string {
	path := filepath.Join(stateDir, accountID+".sync.json")
	body, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var v struct {
		Buf string `json:"get_updates_buf"`
	}
	_ = json.Unmarshal(body, &v)
	return v.Buf
}

// saveSyncBuf persists the long-poll cursor so a restart resumes from where
// the previous run left off (avoiding a flood of replayed messages).
func saveSyncBuf(stateDir, accountID, buf string) error {
	if stateDir == "" {
		return nil
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(stateDir, accountID+".sync.json")
	body, _ := json.Marshal(map[string]string{"get_updates_buf": buf})
	return writeFile0600(path, body)
}

func writeFile0600(path string, body []byte) error {
	return os.WriteFile(path, body, 0o600)
}

func firstNonEmptyStr(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
