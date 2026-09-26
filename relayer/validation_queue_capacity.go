package relayer

import (
	"fmt"
	"sort"
)

// validationQueueMarginThresholdBytes is how little headroom is worth saying
// out loud. Below it, the queues alone could take the process close enough to
// its limit that an ordinary spike finishes the job.
const validationQueueMarginThresholdBytes int64 = 1 << 30

// ValidationQueueService is one service's share of the bound, as the report
// sees it.
type ValidationQueueService struct {
	ServiceID string
	// ConfiguredMiB is what the config asked for, 0 when it asked for nothing.
	ConfiguredMiB int
	// EffectiveBytes is what the relayer will enforce: the configured value, or
	// the default, raised to the floor.
	EffectiveBytes int64
	// FloorBytes is one relay of this service's largest allowed size.
	FloorBytes int64
	// RaisedToFloor says the configured value was below the floor and was
	// raised. That service would otherwise refuse its own traffic.
	RaisedToFloor bool
}

// ValidationQueueReport is what the operator needs to size the container: what
// the validation queues may hold, against what the process is allowed to use.
//
// It is deliberately about the QUEUES and not about the relayer: these bounds
// cover the bodies of HTTP relays served and not yet validated, which is one of
// the things the process holds, not all of them.
type ValidationQueueReport struct {
	// Services are the ones whose relays can actually enter the queue, sorted
	// by service ID so the log line is stable across restarts.
	Services []ValidationQueueService
	// TotalBytes is the sum of their effective bounds: the ceiling those queues
	// can reach together, now that no global bound sits above them.
	TotalBytes int64
	// MemoryLimitBytes is what the process may use, 0 when it could not be
	// determined -- which is reported, never treated as "fits".
	MemoryLimitBytes int64
	// MemorySource names where that number came from, for a reader who has to
	// decide whether to believe it.
	MemorySource string
}

// Fits reports whether the queues fit inside the memory limit. An unknown limit
// is not a fit: it is an unknown, and the caller says so.
func (r ValidationQueueReport) Fits() bool {
	return r.MemoryLimitBytes > 0 && r.TotalBytes <= r.MemoryLimitBytes
}

// MarginBytes is what is left for everything else the process does. Negative
// means the queues alone are over the limit.
func (r ValidationQueueReport) MarginBytes() int64 {
	return r.MemoryLimitBytes - r.TotalBytes
}

// MarginIsThin reports a fit so tight that it is worth saying out loud.
func (r ValidationQueueReport) MarginIsThin() bool {
	return r.Fits() && r.MarginBytes() < validationQueueMarginThresholdBytes
}

// RaisedToFloor returns the services whose configured bound was below their
// floor. Each is a line the operator should see: the number they wrote is not
// the number in force.
func (r ValidationQueueReport) RaisedToFloor() []ValidationQueueService {
	var raised []ValidationQueueService
	for _, svc := range r.Services {
		if svc.RaisedToFloor {
			raised = append(raised, svc)
		}
	}
	return raised
}

// BuildValidationQueueReport computes the report from the config alone, so the
// startup path and `relayer validate` answer with the SAME numbers. Two copies
// of this arithmetic is how a validate command and the binary it validates come
// to disagree.
//
// memoryLimitBytes and memorySource come from the caller (internal/memlimit),
// which is where "how much memory is there" is already answered.
//
// WHICH SERVICES COUNT: those whose EFFECTIVE validation mode is optimistic AND
// whose relays can reach this queue. Only the HTTP handler queues -- WebSocket
// and gRPC relays never enter it -- so counting every optimistic service would
// add bounds that cannot be reached to a number the operator sizes RAM with.
func BuildValidationQueueReport(c *Config, memoryLimitBytes int64, memorySource string) ValidationQueueReport {
	report := ValidationQueueReport{
		MemoryLimitBytes: memoryLimitBytes,
		MemorySource:     memorySource,
	}
	for id, svc := range c.Services {
		if !serviceQueuesForValidation(c, id) {
			continue
		}
		effective := c.ValidationQueueMaxBytes(id)
		floor := c.ValidationQueueFloorBytes(id)
		report.Services = append(report.Services, ValidationQueueService{
			ServiceID:      id,
			ConfiguredMiB:  svc.ValidationQueueMaxMiB,
			EffectiveBytes: effective,
			FloorBytes:     floor,
			RaisedToFloor:  effective == floor && effective > int64(svc.ValidationQueueMaxMiB)<<20,
		})
		report.TotalBytes += effective
	}
	sort.Slice(report.Services, func(i, j int) bool {
		return report.Services[i].ServiceID < report.Services[j].ServiceID
	})
	return report
}

// serviceQueuesForValidation is THE predicate for "this service's relays can
// enter a validation queue". It is one function because it has two readers that
// must never disagree: newValidationQueues, which decides who GETS a queue, and
// BuildValidationQueueReport, which decides who is COUNTED in the memory the
// operator sizes the container with.
//
// While the rule was written out twice they already differed, and not in
// theory: the constructor asked only about the mode, so an optimistic service
// served only over WebSocket or gRPC got a queue and a published ceiling
// series, while the report left it out of its total on purpose. A dashboard
// summing validation_queue_max_bytes then told the operator a LARGER number
// than the startup warning -- which is the very thing the report's own comment
// says it excludes those services to avoid.
func serviceQueuesForValidation(c *Config, serviceID string) bool {
	if c.GetServiceValidationMode(serviceID) != ValidationModeOptimistic {
		return false
	}
	svc, ok := c.Services[serviceID]
	if !ok {
		return false
	}
	return serviceQueuesOnHTTP(svc)
}

// serviceQueuesOnHTTP reports whether this service can put anything in the
// validation queue, which only the HTTP handler feeds.
//
// A service reaches that handler through its HTTP-shaped backends; one whose
// backends are all WebSocket or gRPC is served elsewhere and its queue would
// read zero forever.
func serviceQueuesOnHTTP(svc ServiceConfig) bool {
	for rpcType := range svc.Backends {
		switch rpcType {
		case "websocket", "grpc":
			continue
		default:
			return true
		}
	}
	return false
}

// String renders the report as the operator reads it, with the three actions
// spelled out. A capacity warning that does not say what to do is a number.
func (r ValidationQueueReport) String() string {
	limit := "unknown"
	if r.MemoryLimitBytes > 0 {
		limit = fmt.Sprintf("%d MiB (%s)", r.MemoryLimitBytes>>20, r.MemorySource)
	}
	return fmt.Sprintf(
		"HTTP validation queues may hold up to %d MiB across %d service(s); memory limit %s. "+
			"To change it: raise the container limit or GOMEMLIMIT, lower default_validation_queue_max_mib "+
			"or a service's validation_queue_max_mib, or move services to validation_mode: eager",
		r.TotalBytes>>20, len(r.Services), limit)
}
