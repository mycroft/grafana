package alerting

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/benbjohnson/clock"
	"github.com/grafana/grafana/pkg/bus"
	"github.com/grafana/grafana/pkg/infra/log"
	"github.com/grafana/grafana/pkg/infra/remotecache"
	"github.com/grafana/grafana/pkg/models"
	"github.com/grafana/grafana/pkg/plugins"
	"github.com/grafana/grafana/pkg/registry"
	"github.com/grafana/grafana/pkg/services/rendering"
	"github.com/grafana/grafana/pkg/setting"
	"github.com/opentracing/opentracing-go"
	"github.com/opentracing/opentracing-go/ext"
	tlog "github.com/opentracing/opentracing-go/log"
	"golang.org/x/sync/errgroup"
)

// AlertEngine is the background process that
// schedules alert evaluations and makes sure notifications
// are sent.
type AlertEngine struct {
	RenderService      rendering.Service             `inject:""`
	Bus                bus.Bus                       `inject:""`
	RequestValidator   models.PluginRequestValidator `inject:""`
	DataService        plugins.DataRequestHandler    `inject:""`
	Cfg                *setting.Cfg                  `inject:""`
	RemoteCacheService *remotecache.RemoteCache      `inject:""`

	execQueue     chan *Job
	ticker        *Ticker
	scheduler     scheduler
	evalHandler   evalHandler
	ruleReader    ruleReader
	log           log.Logger
	resultHandler resultHandler
}

type ClusterAlertingInstance struct {
	Instance string
}

func init() {
	registry.RegisterService(&AlertEngine{})
	remotecache.Register(&ClusterAlertingInstance{})
}

// IsDisabled returns true if the alerting service is disable for this instance.
func (e *AlertEngine) IsDisabled() bool {
	return !setting.AlertingEnabled || !setting.ExecuteAlerts || e.Cfg.IsNgAlertEnabled()
}

// Init initializes the AlertingService.
func (e *AlertEngine) Init() error {
	e.ticker = NewTicker(time.Now(), time.Second*0, clock.New(), 1)
	e.execQueue = make(chan *Job, 1000)
	e.scheduler = newScheduler()
	e.evalHandler = NewEvalHandler(e.DataService)
	e.ruleReader = newRuleReader()
	e.log = log.New("alerting.engine")
	e.resultHandler = newResultHandler(e.RenderService)
	return nil
}

// Run starts the alerting service background process.
func (e *AlertEngine) Run(ctx context.Context) error {
	alertGroup, ctx := errgroup.WithContext(ctx)
	alertGroup.Go(func() error { return e.alertingTicker(ctx) })
	alertGroup.Go(func() error { return e.runJobDispatcher(ctx) })

	err := alertGroup.Wait()
	return err
}

func (e *AlertEngine) alertingTicker(grafanaCtx context.Context) error {
	defer func() {
		if err := recover(); err != nil {
			e.log.Error("Scheduler Panic: stopping alertingTicker", "error", err, "stack", log.Stack(1))
		}
	}()

	cluster_alerting_instance := setting.AlertingClusteringInstance

	tickIndex := 0

	for {
		select {
		case <-grafanaCtx.Done():
			return grafanaCtx.Err()
		case tick := <-e.ticker.C:
			// TEMP SOLUTION update rules ever tenth tick
			if tickIndex%10 == 0 {
				e.scheduler.Update(e.ruleReader.fetch())
			}

			schedule_alerts := true
			current_active_instance := cluster_alerting_instance

			if setting.AlertingClusteringEnabled {
				cache_record, err := e.RemoteCacheService.Get("cluster_alerting_instance")
				if err != nil {
					e.log.Warn("Alert Clustering: Could not retrieve the alerting instance", "instance", cluster_alerting_instance, "err", err)
				}

				if cluster_instance_record, ok := cache_record.(*ClusterAlertingInstance); ok {
					current_active_instance = cluster_instance_record.Instance
				}

				if cache_record == nil || current_active_instance == cluster_alerting_instance {
					err = e.RemoteCacheService.Set("cluster_alerting_instance",
						&ClusterAlertingInstance{
							Instance: cluster_alerting_instance,
						},
						time.Second*time.Duration(setting.AlertingClusteringTimeout),
					)

					if err != nil {
						e.log.Warn("Alert Clustering: Could not set the cluster_alerting_instance in cache", "err", err)
					}
				} else {
					schedule_alerts = false
				}
			}

			if schedule_alerts {
				e.scheduler.Tick(tick, e.execQueue)
			} else {
				if tickIndex%10 == 0 {
					e.log.Debug("Alert Clustering enabled but this instance is not marked active: Skipping alerting.",
						"instance",
						cluster_alerting_instance,
						"active",
						current_active_instance,
					)
				}
			}

			tickIndex++
		}
	}
}

func (e *AlertEngine) runJobDispatcher(grafanaCtx context.Context) error {
	dispatcherGroup, alertCtx := errgroup.WithContext(grafanaCtx)

	for {
		select {
		case <-grafanaCtx.Done():
			return dispatcherGroup.Wait()
		case job := <-e.execQueue:
			dispatcherGroup.Go(func() error { return e.processJobWithRetry(alertCtx, job) })
		}
	}
}

var (
	unfinishedWorkTimeout = time.Second * 5
)

func (e *AlertEngine) processJobWithRetry(grafanaCtx context.Context, job *Job) error {
	defer func() {
		if err := recover(); err != nil {
			e.log.Error("Alert Panic", "error", err, "stack", log.Stack(1))
		}
	}()

	cancelChan := make(chan context.CancelFunc, setting.AlertingMaxAttempts*2)
	attemptChan := make(chan int, 1)

	// Initialize with first attemptID=1
	attemptChan <- 1
	job.SetRunning(true)

	for {
		select {
		case <-grafanaCtx.Done():
			// In case grafana server context is cancel, let a chance to job processing
			// to finish gracefully - by waiting a timeout duration - before forcing its end.
			unfinishedWorkTimer := time.NewTimer(unfinishedWorkTimeout)
			select {
			case <-unfinishedWorkTimer.C:
				return e.endJob(grafanaCtx.Err(), cancelChan, job)
			case <-attemptChan:
				return e.endJob(nil, cancelChan, job)
			}
		case attemptID, more := <-attemptChan:
			if !more {
				return e.endJob(nil, cancelChan, job)
			}
			go e.processJob(attemptID, attemptChan, cancelChan, job)
		}
	}
}

func (e *AlertEngine) endJob(err error, cancelChan chan context.CancelFunc, job *Job) error {
	job.SetRunning(false)
	close(cancelChan)
	for cancelFn := range cancelChan {
		cancelFn()
	}
	return err
}

func (e *AlertEngine) processJob(attemptID int, attemptChan chan int, cancelChan chan context.CancelFunc, job *Job) {
	defer func() {
		if err := recover(); err != nil {
			e.log.Error("Alert Panic", "error", err, "stack", log.Stack(1))
		}
	}()

	alertCtx, cancelFn := context.WithTimeout(context.Background(), setting.AlertingEvaluationTimeout)
	cancelChan <- cancelFn
	span := opentracing.StartSpan("alert execution")
	alertCtx = opentracing.ContextWithSpan(alertCtx, span)

	evalContext := NewEvalContext(alertCtx, job.Rule, e.RequestValidator)
	evalContext.Ctx = alertCtx

	go func() {
		defer func() {
			if err := recover(); err != nil {
				e.log.Error("Alert Panic", "error", err, "stack", log.Stack(1))
				ext.Error.Set(span, true)
				span.LogFields(
					tlog.Error(fmt.Errorf("%v", err)),
					tlog.String("message", "failed to execute alert rule. panic was recovered."),
				)
				span.Finish()
				close(attemptChan)
			}
		}()

		e.evalHandler.Eval(evalContext)

		span.SetTag("alertId", evalContext.Rule.ID)
		span.SetTag("dashboardId", evalContext.Rule.DashboardID)
		span.SetTag("firing", evalContext.Firing)
		span.SetTag("nodatapoints", evalContext.NoDataFound)
		span.SetTag("attemptID", attemptID)

		if evalContext.Error != nil {
			ext.Error.Set(span, true)
			span.LogFields(
				tlog.Error(evalContext.Error),
				tlog.String("message", "alerting execution attempt failed"),
			)
			if attemptID < setting.AlertingMaxAttempts {
				span.Finish()
				e.log.Debug("Job Execution attempt triggered retry", "timeMs", evalContext.GetDurationMs(), "alertId", evalContext.Rule.ID, "name", evalContext.Rule.Name, "firing", evalContext.Firing, "attemptID", attemptID)
				attemptChan <- (attemptID + 1)
				return
			}
		}

		// create new context with timeout for notifications
		resultHandleCtx, resultHandleCancelFn := context.WithTimeout(context.Background(), setting.AlertingNotificationTimeout)
		cancelChan <- resultHandleCancelFn

		// override the context used for evaluation with a new context for notifications.
		// This makes it possible for notifiers to execute when datasources
		// don't respond within the timeout limit. We should rewrite this so notifications
		// don't reuse the evalContext and get its own context.
		evalContext.Ctx = resultHandleCtx
		evalContext.Rule.State = evalContext.GetNewState()
		if err := e.resultHandler.handle(evalContext); err != nil {
			switch {
			case errors.Is(err, context.Canceled):
				e.log.Debug("Result handler returned context.Canceled")
			case errors.Is(err, context.DeadlineExceeded):
				e.log.Debug("Result handler returned context.DeadlineExceeded")
			default:
				e.log.Error("Failed to handle result", "err", err)
			}
		}

		span.Finish()
		e.log.Debug("Job Execution completed", "timeMs", evalContext.GetDurationMs(), "alertId", evalContext.Rule.ID, "name", evalContext.Rule.Name, "firing", evalContext.Firing, "attemptID", attemptID)
		close(attemptChan)
	}()
}
