package main

import (
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jellydator/ttlcache/v3"
)

const (
	webhookEndpoint = "/unifi/webhook"
	statusEndpoint  = "/lock/status"

	eventType             = "access.door.unlock"
	eventMaxClockSkew     = 30 * time.Second
	eventReplayTTL        = 5 * time.Minute
	eventReplayMaxEntries = 10000
	eventMaxBodyBytes     = 1048576

	haDeliveryQueueSize   = 100
	haDeliveryMaxAttempts = 15
	haDeliveryInitialWait = 2 * time.Second
	haDeliveryMaxWait     = 1 * time.Minute
)

type config struct {
	ListenAddress        string   `env:"LISTEN_ADDRESS" envDefault:":8080"`
	UIWebhookSecret      string   `env:"UNIFI_WEBHOOK_SECRET,required"`
	UIAllowedPolicyIDs   []string `env:"UNIFI_ALLOWED_POLICY_IDS" envSeparator:","`
	UIAllowedActorIDs    []string `env:"UNIFI_ALLOWED_ACTOR_IDS" envSeparator:","`
	UIAllowedDeviceIDs   []string `env:"UNIFI_ALLOWED_DEVICE_IDS" envSeparator:","`
	HABaseURL            string   `env:"HA_BASE_URL,required"`
	HAToken              string   `env:"HA_TOKEN,required"`
	HAScriptEntityID     string   `env:"HA_SCRIPT_ENTITY_ID,required"`
	LockStatusSecret     string   `env:"LOCK_STATUS_SECRET"`
}

type signatureHeader struct {
	Timestamp time.Time
	V1        []byte
}

type unifiEvent struct {
	Event         string `json:"event"`
	EventObjectID string `json:"event_object_id"`
	Data          struct {
		Actor *struct {
			ID string `json:"id"`
		} `json:"actor"`
		Location *struct {
			ID string `json:"id"`
		} `json:"location"`
		Device *struct {
			ID string `json:"id"`
		} `json:"device"`
		Object struct {
			Result   string `json:"result"`
			PolicyID string `json:"policy_id"`
		} `json:"object"`
	} `json:"data"`
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	logger := log.New(log.Writer(), "", log.LstdFlags|log.LUTC)
	replay := ttlcache.New(
		ttlcache.WithTTL[string, struct{}](eventReplayTTL),
		ttlcache.WithCapacity[string, struct{}](uint64(eventReplayMaxEntries)),
	)
	go replay.Start()
	defer replay.Stop()

	s := &server{
		cfg:              *cfg,
		client:           newHAClient(),
		replay:           replay,
		logger:           logger,
		allowedPolicyIDs: makeStringSet(cfg.UIAllowedPolicyIDs),
		allowedActorIDs:  makeStringSet(cfg.UIAllowedActorIDs),
		allowedDeviceIDs: makeStringSet(cfg.UIAllowedDeviceIDs),
		lockStates:       make(map[string]lockState),
		haScriptURL:      buildHAScriptURL(cfg.HABaseURL),
		haEvents:         make(chan haDelivery, haDeliveryQueueSize),
	}
	go s.deliverHomeAssistantEvents()

	r := chi.NewRouter()
	r.Use(middleware.RealIP, middleware.RequestID, middleware.Logger, middleware.Recoverer, middleware.Timeout(15*time.Second))
	r.Post(webhookEndpoint, s.handleWebhook)
	if s.cfg.LockStatusSecret != "" {
		r.Post(statusEndpoint, s.handleStatus)
	}

	httpServer := &http.Server{
		Addr:              s.cfg.ListenAddress,
		Handler:           r,
		ReadHeaderTimeout: 5 * time.Second,
	}

	logger.Printf("starting service on %s", s.cfg.ListenAddress)

	err = httpServer.ListenAndServe()
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Fatalf("service returned error: %v", err)
	}
}
