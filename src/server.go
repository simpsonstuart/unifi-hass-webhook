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
	"strings"
	"sync"
	"time"

	"github.com/jellydator/ttlcache/v3"
)

type server struct {
	cfg              config
	client           *http.Client
	replay           *ttlcache.Cache[string, struct{}]
	logger           *log.Logger
	allowedPolicyIDs map[string]struct{}
	allowedActorIDs  map[string]struct{}
	allowedDeviceIDs map[string]struct{}
	stateMu          sync.Mutex
	lockStates       map[string]lockState
	haScriptURL      string
	haEvents         chan haDelivery
}

type haDelivery struct {
	event         unifiEvent
	rawBody       []byte
	sig           signatureHeader
	receivedAt    time.Time
	action        lockAction
	previousState lockState
	newState      lockState
	stateSource   string
}

type lockAction string

const (
	lockActionUnlock lockAction = "unlock"
	lockActionLock   lockAction = "lock"
)

type lockState string

const (
	lockStateUnknown  lockState = "unknown"
	lockStateLocked   lockState = "locked"
	lockStateUnlocked lockState = "unlocked"
)

type lockStatusUpdate struct {
	DeviceID string `json:"device_id"`
	State    string `json:"state"`
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
		s.logger.Printf(
			"event not allowed: reason=%v event=%s event_object_id=%s policy_id=%s actor_id=%s device_id=%s",
			err,
			event.Event,
			event.EventObjectID,
			event.Data.Object.PolicyID,
			valueOrEmpty(event.Data.Actor, func(actor *struct{ ID string `json:"id"` }) string { return actor.ID }),
			valueOrEmpty(event.Data.Device, func(device *struct{ ID string `json:"id"` }) string { return device.ID }),
		)
		w.WriteHeader(http.StatusAccepted)
		return
	}

	replayKey := makeReplayKey(event, *sig, body)
	if s.replay.Get(replayKey) != nil {
		s.logger.Printf("replay detected for event: %s", replayKey)
		w.WriteHeader(http.StatusAccepted)
		return
	}

	action, previousState, newState := s.nextLockAction(event.Data.Device.ID)
	delivery := haDelivery{
		event:         event,
		rawBody:       body,
		sig:           *sig,
		receivedAt:    now,
		action:        action,
		previousState: previousState,
		newState:      newState,
		stateSource:   "optimistic",
	}

	select {
	case s.haEvents <- delivery:
	default:
		s.revertLockStateIfCurrent(event.Data.Device.ID, newState, previousState)
		s.logger.Printf("Home Assistant delivery queue full")
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}

	s.replay.Set(replayKey, struct{}{}, ttlcache.DefaultTTL)
	s.logger.Printf(
		"accepted event=%s event_object_id=%s policy_id=%s actor_id=%s device_id=%s action=%s previous_state=%s new_state=%s queued_for_home_assistant=true",
		event.Event,
		event.EventObjectID,
		event.Data.Object.PolicyID,
		valueOrEmpty(event.Data.Actor, func(actor *struct{ ID string `json:"id"` }) string { return actor.ID }),
		valueOrEmpty(event.Data.Device, func(device *struct{ ID string `json:"id"` }) string { return device.ID }),
		action,
		previousState,
		newState,
	)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":         "ok",
		"event":          event.Event,
		"action":         action,
		"previous_state": previousState,
		"new_state":      newState,
	})
}

func (s *server) isEventAllowed(event unifiEvent) (bool, error) {
	if event.Event != eventType {
		return false, errors.New("event type mismatch")
	}

	if event.Data.Object.Result != "Access Granted" {
		return false, errors.New("result check failed")
	}

	if !stringSetContains(s.allowedPolicyIDs, event.Data.Object.PolicyID) {
		return false, errors.New("policy_id not allowed")
	}

	if event.Data.Actor == nil {
		return false, errors.New("missing actor")
	}

	if !stringSetContains(s.allowedActorIDs, event.Data.Actor.ID) {
		return false, errors.New("actor_id not allowed")
	}

	if event.Data.Device == nil {
		return false, errors.New("missing device")
	}

	if !stringSetContains(s.allowedDeviceIDs, event.Data.Device.ID) {
		return false, errors.New("device_id not allowed")
	}

	return true, nil
}

func (s *server) nextLockAction(deviceID string) (lockAction, lockState, lockState) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()

	previousState := s.lockStates[deviceID]
	if previousState == "" {
		previousState = lockStateUnknown
	}

	action := lockActionUnlock
	newState := lockStateUnlocked
	if previousState == lockStateUnlocked {
		action = lockActionLock
		newState = lockStateLocked
	}

	s.lockStates[deviceID] = newState
	return action, previousState, newState
}

func (s *server) setLockState(deviceID string, state lockState) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	s.lockStates[deviceID] = state
}

func (s *server) revertLockStateIfCurrent(deviceID string, currentState, previousState lockState) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if s.lockStates[deviceID] == currentState {
		s.lockStates[deviceID] = previousState
	}
}

func parseLockState(value string) (lockState, bool) {
	switch lockState(strings.ToLower(strings.TrimSpace(value))) {
	case lockStateLocked:
		return lockStateLocked, true
	case lockStateUnlocked:
		return lockStateUnlocked, true
	default:
		return "", false
	}
}

func (s *server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Lock-Status-Secret") != s.cfg.LockStatusSecret {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	var update lockStatusUpdate
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&update); err != nil {
		s.logger.Printf("invalid lock status payload: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	deviceID := strings.TrimSpace(update.DeviceID)
	if deviceID == "" {
		s.logger.Printf("lock status update missing device_id")
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	state, ok := parseLockState(update.State)
	if !ok {
		s.logger.Printf("lock status update has invalid state: %s", update.State)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	s.setLockState(deviceID, state)
	s.logger.Printf("updated lock state from status endpoint: device_id=%s state=%s", deviceID, state)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "device_id": deviceID, "state": state})
}

func (s *server) deliverHomeAssistantEvents() {
	for delivery := range s.haEvents {
		reqBody, err := s.buildHomeAssistantRequestBody(delivery)
		if err != nil {
			s.logger.Printf(
				"failed to build Home Assistant request body: event=%s event_object_id=%s action=%s error=%v",
				delivery.event.Event,
				delivery.event.EventObjectID,
				delivery.action,
				err,
			)
			s.revertLockStateIfCurrent(delivery.event.Data.Device.ID, delivery.newState, delivery.previousState)
			continue
		}

		wait := haDeliveryInitialWait

		for attempt := 1; attempt <= haDeliveryMaxAttempts; attempt++ {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			err := s.callHomeAssistant(ctx, reqBody)
			cancel()
			if err == nil {
				s.logger.Printf(
					"delivered event to Home Assistant: event=%s event_object_id=%s action=%s attempt=%d",
					delivery.event.Event,
					delivery.event.EventObjectID,
					delivery.action,
					attempt,
				)
				break
			}

			if attempt == haDeliveryMaxAttempts {
				s.logger.Printf(
					"failed to deliver event to Home Assistant after %d attempts: event=%s event_object_id=%s action=%s error=%v",
					attempt,
					delivery.event.Event,
					delivery.event.EventObjectID,
					delivery.action,
					err,
				)
				s.revertLockStateIfCurrent(delivery.event.Data.Device.ID, delivery.newState, delivery.previousState)
				break
			}

			s.logger.Printf(
				"failed to deliver event to Home Assistant, retrying: event=%s event_object_id=%s action=%s attempt=%d wait=%s error=%v",
				delivery.event.Event,
				delivery.event.EventObjectID,
				delivery.action,
				attempt,
				wait,
				err,
			)
			time.Sleep(wait)
			wait *= 2
			if wait > haDeliveryMaxWait {
				wait = haDeliveryMaxWait
			}
		}
	}
}

func (s *server) buildHomeAssistantRequestBody(delivery haDelivery) ([]byte, error) {
	return json.Marshal(map[string]any{
		"entity_id": s.cfg.HAScriptEntityID,
		"variables": map[string]any{
			"unifi_event_name":      delivery.event.Event,
			"unifi_event_object_id": delivery.event.EventObjectID,
			"unifi_signature_time":  delivery.sig.Timestamp.UTC().Format(time.RFC3339),
			"unifi_received_at":     delivery.receivedAt.UTC().Format(time.RFC3339),
			"unifi_lock_action":     delivery.action,
			"unifi_previous_state":  delivery.previousState,
			"unifi_new_state":       delivery.newState,
			"unifi_state_source":    delivery.stateSource,
			"unifi_event":           delivery.event,
			"unifi_event_json":      string(delivery.rawBody),
		},
	})
}

func (s *server) callHomeAssistant(ctx context.Context, reqBody []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.haScriptURL, bytes.NewReader(reqBody))
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
