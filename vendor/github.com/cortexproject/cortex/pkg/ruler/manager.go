package ruler

import (
	"context"
	"net/http"
	"sync"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	ot "github.com/opentracing/opentracing-go"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/prometheus/config"
	"github.com/prometheus/prometheus/notifier"
	promRules "github.com/prometheus/prometheus/rules"
	"github.com/weaveworks/common/user"
	"golang.org/x/net/context/ctxhttp"

	store "github.com/cortexproject/cortex/pkg/ruler/rules"
	"github.com/cortexproject/cortex/pkg/util"
)

type DefaultMultiTenantManager struct {
	cfg            Config
	notifierCfg    *config.Config
	managerFactory ManagerFactory

	mapper *mapper

	// Structs for holding per-user Prometheus rules Managers
	// and a corresponding metrics struct
	userManagerMtx     sync.Mutex
	userManagers       map[string]*promRules.Manager
	userManagerMetrics *ManagerMetrics

	// Per-user notifiers with separate queues.
	notifiersMtx sync.Mutex
	notifiers    map[string]*rulerNotifier

	managersTotal prometheus.Gauge
	registry      prometheus.Registerer
	logger        log.Logger
}

func NewDefaultMultiTenantManager(cfg Config, managerFactory ManagerFactory, reg prometheus.Registerer, logger log.Logger) (*DefaultMultiTenantManager, error) {
	ncfg, err := buildNotifierConfig(&cfg)
	if err != nil {
		return nil, err
	}

	userManagerMetrics := NewManagerMetrics()
	if reg != nil {
		reg.MustRegister(userManagerMetrics)
	}

	return &DefaultMultiTenantManager{
		cfg:                cfg,
		notifierCfg:        ncfg,
		managerFactory:     managerFactory,
		notifiers:          map[string]*rulerNotifier{},
		mapper:             newMapper(cfg.RulePath, logger),
		userManagers:       map[string]*promRules.Manager{},
		userManagerMetrics: userManagerMetrics,
		managersTotal: promauto.With(reg).NewGauge(prometheus.GaugeOpts{
			Namespace: "cortex",
			Name:      "ruler_managers_total",
			Help:      "Total number of managers registered and running in the ruler",
		}),
		registry: reg,
		logger:   logger,
	}, nil
}

func (r *DefaultMultiTenantManager) SyncRuleGroups(ctx context.Context, ruleGroups map[string]store.RuleGroupList) {
	// A lock is taken to ensure if this function is called concurrently, then each call
	// returns after the call map files and check for updates
	r.userManagerMtx.Lock()
	defer r.userManagerMtx.Unlock()

	for userID, ruleGroup := range ruleGroups {
		r.syncRulesToManager(ctx, userID, ruleGroup)
	}

	// Check for deleted users and remove them
	for userID, mngr := range r.userManagers {
		if _, exists := ruleGroups[userID]; !exists {
			go mngr.Stop()
			delete(r.userManagers, userID)
			level.Info(r.logger).Log("msg", "deleting rule manager", "user", userID)
		}
	}

	r.managersTotal.Set(float64(len(r.userManagers)))
}

// syncRulesToManager maps the rule files to disk, detects any changes and will create/update the
// the users Prometheus Rules Manager.
func (r *DefaultMultiTenantManager) syncRulesToManager(ctx context.Context, user string, groups store.RuleGroupList) {
	// Map the files to disk and return the file names to be passed to the users manager if they
	// have been updated
	update, files, err := r.mapper.MapRules(user, groups.Formatted())
	if err != nil {
		level.Error(r.logger).Log("msg", "unable to map rule files", "user", user, "err", err)
		return
	}

	if update {
		level.Debug(r.logger).Log("msg", "updating rules", "user", "user")
		configUpdatesTotal.WithLabelValues(user).Inc()
		manager, exists := r.userManagers[user]
		if !exists {
			manager, err = r.newManager(ctx, user)
			if err != nil {
				configUpdateFailuresTotal.WithLabelValues(user, "rule-manager-creation-failure").Inc()
				level.Error(r.logger).Log("msg", "unable to create rule manager", "user", user, "err", err)
				return
			}
			// manager.Run() starts running the manager and blocks until Stop() is called.
			// Hence run it as another goroutine.
			go manager.Run()
			r.userManagers[user] = manager
		}
		err = manager.Update(r.cfg.EvaluationInterval, files, nil)
		if err != nil {
			configUpdateFailuresTotal.WithLabelValues(user, "rules-update-failure").Inc()
			level.Error(r.logger).Log("msg", "unable to update rule manager", "user", user, "err", err)
			return
		}
	}
}

// newManager creates a prometheus rule manager wrapped with a user id
// configured storage, appendable, notifier, and instrumentation
func (r *DefaultMultiTenantManager) newManager(ctx context.Context, userID string) (*promRules.Manager, error) {
	notifier, err := r.getOrCreateNotifier(userID)
	if err != nil {
		return nil, err
	}

	// Create a new Prometheus registry and register it within
	// our metrics struct for the provided user.
	reg := prometheus.NewRegistry()
	r.userManagerMetrics.AddUserRegistry(userID, reg)

	logger := log.With(r.logger, "user", userID)
	return r.managerFactory(ctx, userID, notifier, logger, reg), nil
}

func (r *DefaultMultiTenantManager) getOrCreateNotifier(userID string) (*notifier.Manager, error) {
	r.notifiersMtx.Lock()
	defer r.notifiersMtx.Unlock()

	n, ok := r.notifiers[userID]
	if ok {
		return n.notifier, nil
	}

	reg := prometheus.WrapRegistererWith(prometheus.Labels{"user": userID}, r.registry)
	reg = prometheus.WrapRegistererWithPrefix("cortex_", reg)
	n = newRulerNotifier(&notifier.Options{
		QueueCapacity: r.cfg.NotificationQueueCapacity,
		Registerer:    reg,
		Do: func(ctx context.Context, client *http.Client, req *http.Request) (*http.Response, error) {
			// Note: The passed-in context comes from the Prometheus notifier
			// and does *not* contain the userID. So it needs to be added to the context
			// here before using the context to inject the userID into the HTTP request.
			ctx = user.InjectOrgID(ctx, userID)
			if err := user.InjectOrgIDIntoHTTPRequest(ctx, req); err != nil {
				return nil, err
			}
			// Jaeger complains the passed-in context has an invalid span ID, so start a new root span
			sp := ot.GlobalTracer().StartSpan("notify", ot.Tag{Key: "organization", Value: userID})
			defer sp.Finish()
			ctx = ot.ContextWithSpan(ctx, sp)
			_ = ot.GlobalTracer().Inject(sp.Context(), ot.HTTPHeaders, ot.HTTPHeadersCarrier(req.Header))
			return ctxhttp.Do(ctx, client, req)
		},
	}, util.Logger)

	go n.run()

	// This should never fail, unless there's a programming mistake.
	if err := n.applyConfig(r.notifierCfg); err != nil {
		return nil, err
	}

	r.notifiers[userID] = n
	return n.notifier, nil
}

func (r *DefaultMultiTenantManager) GetRules(userID string) []*promRules.Group {
	var groups []*promRules.Group
	r.userManagerMtx.Lock()
	if mngr, exists := r.userManagers[userID]; exists {
		groups = mngr.RuleGroups()
	}
	r.userManagerMtx.Unlock()
	return groups
}

func (r *DefaultMultiTenantManager) Stop() {
	r.notifiersMtx.Lock()
	for _, n := range r.notifiers {
		n.stop()
	}
	r.notifiersMtx.Unlock()

	level.Info(r.logger).Log("msg", "stopping user managers")
	wg := sync.WaitGroup{}
	r.userManagerMtx.Lock()
	for user, manager := range r.userManagers {
		level.Debug(r.logger).Log("msg", "shutting down user  manager", "user", user)
		wg.Add(1)
		go func(manager *promRules.Manager, user string) {
			manager.Stop()
			wg.Done()
			level.Debug(r.logger).Log("msg", "user manager shut down", "user", user)
		}(manager, user)
	}
	wg.Wait()
	r.userManagerMtx.Unlock()
	level.Info(r.logger).Log("msg", "all user managers stopped")
}