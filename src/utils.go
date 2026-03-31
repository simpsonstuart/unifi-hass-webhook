package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	env "github.com/caarlos0/env/v11"
)

func newHAClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	return &http.Client{Timeout: 4 * time.Second, Transport: transport}
}

func parseSignatureHeader(header string) (*signatureHeader, error) {
	if strings.TrimSpace(header) == "" {
		return nil, errors.New("missing Signature header")
	}

	var (
		gotTimestamp bool
		gotV1        bool
		result       signatureHeader
	)

	for _, pair := range strings.Split(header, ",") {
		kv := strings.SplitN(strings.TrimSpace(pair), "=", 2)
		if len(kv) != 2 {
			return nil, errors.New("invalid Signature header format")
		}

		switch strings.TrimSpace(kv[0]) {
		case "t":
			t, err := strconv.ParseInt(strings.TrimSpace(kv[1]), 10, 64)
			if err != nil {
				return nil, errors.New("invalid Signature timestamp")
			}
			result.Timestamp = time.Unix(t, 0).UTC()
			gotTimestamp = true
		case "v1":
			v1, err := hex.DecodeString(strings.TrimSpace(kv[1]))
			if err != nil {
				return nil, errors.New("invalid Signature v1")
			}
			result.V1 = v1
			gotV1 = true
		}
	}

	if !gotTimestamp || !gotV1 {
		return nil, errors.New("incomplete Signature header")
	}

	return &result, nil
}

func validateSignature(payload []byte, sig signatureHeader, secret string) error {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(sig.Timestamp.Unix(), 10)))
	mac.Write([]byte("."))
	mac.Write(payload)
	if subtle.ConstantTimeCompare(mac.Sum(nil), sig.V1) != 1 {
		return errors.New("invalid signature")
	}
	return nil
}

func withinClockSkew(now, ts time.Time, maxSkew time.Duration) bool {
	diff := now.Sub(ts)
	if diff < 0 {
		diff = -diff
	}
	return diff <= maxSkew
}

func makeReplayKey(event unifiEvent, sig signatureHeader, body []byte) string {
	if event.EventObjectID != "" {
		return event.Event + ":" + event.EventObjectID
	}
	hash := sha256.Sum256(body)
	return event.Event + ":" + sig.Timestamp.UTC().Format(time.RFC3339) + ":" + hex.EncodeToString(hash[:])
}

func loadConfig() (*config, error) {
	cfg := config{}

	err := env.Parse(&cfg)
	if err != nil {
		return nil, err
	}

	return &cfg, nil
}

func valueOrEmpty[T any](value *T, pick func(*T) string) string {
	if value == nil {
		return ""
	}

	return pick(value)
}
