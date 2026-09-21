package adminweb

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/notify"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

// The secrets inside the channel settings are sealed with the master key,
// under an AAD that names the setting and the field, so a ciphertext cannot
// be moved from one field to another.
func channelAAD(field string) string { return repo.ChannelSecretAAD(field) }

func (s *Server) sealSecret(ctx context.Context, field, plaintext string) (repo.Sealed, error) {
	if plaintext == "" {
		return repo.Sealed{}, nil
	}
	blob, version, err := s.dbm.ring.Seal(ctx, []byte(plaintext), channelAAD(field))
	if err != nil {
		return repo.Sealed{}, err
	}
	return repo.Sealed{Ciphertext: blob, KeyVersion: version}, nil
}

func (s *Server) openSecret(ctx context.Context, field string, sealed repo.Sealed) (string, error) {
	if !sealed.IsSet() {
		return "", nil
	}
	plain, err := s.dbm.ring.Open(ctx, sealed.Ciphertext, sealed.KeyVersion, channelAAD(field))
	if err != nil {
		return "", fmt.Errorf("%s: %w", field, err)
	}
	return string(plain), nil
}

// alertChannels builds the enabled channels from the stored settings. It is
// read per run, so a change on the settings page takes effect at once.
func (s *Server) alertChannels(ctx context.Context) ([]notify.Channel, error) {
	cfg, _, err := repo.LoadChannelSettings(ctx, s.dbm.store.Settings())
	if err != nil {
		return nil, err
	}
	return s.channelsFrom(ctx, cfg, true)
}

// channelsFrom turns settings into channels. onlyEnabled false is for the
// "send a test" button, which wants the channel whether or not it is on.
func (s *Server) channelsFrom(ctx context.Context, cfg repo.ChannelSettings, onlyEnabled bool) ([]notify.Channel, error) {
	var out []notify.Channel
	if cfg.Webhook.URL != "" && (cfg.Webhook.Enabled || !onlyEnabled) {
		secret, err := s.openSecret(ctx, "webhook_secret", cfg.Webhook.Secret)
		if err != nil {
			return nil, err
		}
		out = append(out, notify.Webhook{URL: cfg.Webhook.URL, Format: cfg.Webhook.Format, Secret: secret,
			Client: &http.Client{Timeout: 15 * time.Second}})
	}
	if cfg.SMTP.Host != "" && (cfg.SMTP.Enabled || !onlyEnabled) {
		password, err := s.openSecret(ctx, "smtp_password", cfg.SMTP.Password)
		if err != nil {
			return nil, err
		}
		out = append(out, notify.SMTP{Host: cfg.SMTP.Host, Port: cfg.SMTP.Port, Username: cfg.SMTP.Username, Password: password,
			From: cfg.SMTP.From, To: cfg.SMTP.To, InsecureNoTLS: isLoopback(cfg.SMTP.Host)})
	}
	return out, nil
}

func isLoopback(host string) bool {
	return host == "localhost" || strings.HasPrefix(host, "127.")
}

// consoleURL is the address put into messages, from the public host.
func (s *Server) consoleURL() string {
	if s.opts.PublicHost == "" {
		return ""
	}
	return "https://" + s.opts.PublicHost
}
