package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jellydator/ttlcache/v3"
)

type server struct {
	cfg    config
	client *http.Client
	replay *ttlcache.Cache[string, struct{}]
	logger *log.Logger
}

func (s *server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, eventMaxBodyBytes+1))
	if err != nil {
		s.logger.Printf("failed to read request body: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	if int64(len(body)) > eventMaxBodyBytes {
		s.logger.Printf("request body too large: %d bytes", len(body))
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		return
	}

	if len(body) == 0 {
		s.logger.Printf("empty request body")
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	sig, err := parseSignatureHeader(r.Header.Get("Signature"))
	if err != nil {
		s.logger.Printf("invalid Signature header: %v", err)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	now := time.Now().UTC()
	if !withinClockSkew(now, sig.Timestamp, eventMaxClockSkew) {
		s.logger.Printf("timestamp outside of allowed clock skew: now=%s, sig=%s", now.Format(time.RFC3339), sig.Timestamp.Format(time.RFC3339))
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	if err := validateSignature(body, *sig, s.cfg.UIWebhookSecret); err != nil {
		s.logger.Printf("signature validation failed: %v", err)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	var event unifiEvent
	if err := json.Unmarshal(body, &event); err != nil {
		s.logger.Printf("invalid JSON payload: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	allowed, err := s.isEventAllowed(event)
	if !allowed {
		s.logger.Printf("event not allowed: %v", err)
		w.WriteHeader(http.StatusAccepted)
		return
	}

	replayKey := makeReplayKey(event, *sig, body)
	if s.replay.Get(replayKey) != nil {
		s.logger.Printf("replay detected for event: %s", replayKey)
		w.WriteHeader(http.StatusAccepted)
		return
	}

	if err := s.callHomeAssistant(r.Context(), event, body, *sig); err != nil {
		s.logger.Printf("failed to call Home Assistant: %v", err)
		w.WriteHeader(http.StatusBadGateway)
		return
	}

	s.replay.Set(replayKey, struct{}{}, ttlcache.DefaultTTL)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "event": event.Event})
}

func (s *server) isEventAllowed(event unifiEvent) (bool, error) {
	if event.Event != eventType {
		return false, errors.New("event type mismatch")
	}

	if event.Data.Object.Result != "Access Granted" {
		return false, errors.New("result check failed")
	}

	if !slices.Contains(s.cfg.UIAllowedPolicyIDs, event.Data.Object.PolicyID) {
		return false, errors.New("policy_id not allowed")
	}

	if !slices.Contains(s.cfg.UIAllowedActorIDs, event.Data.Actor.ID) {
		return false, errors.New("actor_id not allowed")
	}

	if !slices.Contains(s.cfg.UIAllowedDeviceIDs, event.Data.Device.ID) {
		return false, errors.New("device_id not allowed")
	}

	return true, nil
}

func (s *server) callHomeAssistant(ctx context.Context, event unifiEvent, rawBody []byte, sig signatureHeader) error {
	reqBody, err := json.Marshal(map[string]any{
		"entity_id": s.cfg.HAScriptEntityID,
		"variables": map[string]any{
			"unifi_event_name":      event.Event,
			"unifi_event_object_id": event.EventObjectID,
			"unifi_signature_time":  sig.Timestamp.UTC().Format(time.RFC3339),
			"unifi_received_at":     time.Now().UTC().Format(time.RFC3339),
			"unifi_event":           event,
			"unifi_event_json":      string(rawBody),
		},
	})
	if err != nil {
		return fmt.Errorf("failed to marshal HA request: %w", err)
	}

	url := strings.TrimRight(s.cfg.HABaseURL, "/") + "/api/services/script/turn_on"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("failed to build HA request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+s.cfg.HAToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to execute HA request: %w", err)
	}

	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("unexpected HA status %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}

	return nil
}
