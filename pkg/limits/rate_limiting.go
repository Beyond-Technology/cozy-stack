package limits

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"time"

	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/prefixer"
	"github.com/go-redis/redis/v8"
)

// CounterType os an enum for the type of counters used by rate-limiting.
type CounterType int

// ErrRateLimitReached is the error returned when we were under the limit
// before the check, and reach the limit.
var ErrRateLimitReached = errors.New("Rate limit reached")

// ErrRateLimitExceeded is the error returned when the limit was already
// reached before the check.
var ErrRateLimitExceeded = errors.New("Rate limit exceeded")

const (
	// AuthType is used for counting the number of login attempts.
	AuthType CounterType = iota
	// TwoFactorGenerationType is used for counting the number of times a 2FA
	// is generated.
	TwoFactorGenerationType
	// TwoFactorType is used for counting the number of 2FA attempts.
	TwoFactorType
	// OAuthClientType is used for counting the number of OAuth clients.
	// creations/updates.
	OAuthClientType
	// SharingInviteType is used for counting the number of sharing invitations
	// sent to a given instance.
	SharingInviteType
	// SharingPublicLinkType is used for counting the number of public sharing
	// link consultations
	SharingPublicLinkType
	// JobThumbnailType is used for counting the number of thumbnail jobs
	// executed by an instance
	JobThumbnailType
	// JobShareTrackType is used for counting the number of updates of the
	// io.cozy.shared database
	JobShareTrackType
	// JobShareReplicateType is used for counting the number of replications
	JobShareReplicateType
	// JobShareUploadType is used for counting the file uploads
	JobShareUploadType
	// JobKonnectorType is used for counting the number of konnector executions
	JobKonnectorType
	// JobZipType is used for cozies exports
	JobZipType
	// JobSendMailType is used for mail sending
	JobSendMailType
	// JobServiceType is used for generic services
	// Ex: categorization or matching for banking
	JobServiceType
	// JobNotificationType is used for mobile notifications pushing
	JobNotificationType
	// SendHintByMail is used for sending the password hint by email
	SendHintByMail
	// JobNotesPersistType is used for saving notes to the VFS
	JobNotesPersistType
	// JobClientType is used for the jobs associated to a @client trigger
	JobClientType
	// ExportType is used for creating an export of the data
	ExportType
	// WebhookTriggerType is used for calling a webhook trigger
	WebhookTriggerType
	// JobCleanClientType is used for cleaning unused OAuth clients
	JobCleanClientType
)

type counterConfig struct {
	Prefix string
	Limit  int64
	Period time.Duration
}

var configs = []counterConfig{
	// AuthType
	{
		Prefix: "auth",
		Limit:  1000,
		Period: 1 * time.Hour,
	},
	// TwoFactorGenerationType
	{
		Prefix: "two-factor-generation",
		Limit:  20,
		Period: 1 * time.Hour,
	},
	// TwoFactorType
	{
		Prefix: "two-factor",
		Limit:  10,
		Period: 5 * time.Minute,
	},
	// OAuthClientType
	{
		Prefix: "oauth-client",
		Limit:  20,
		Period: 1 * time.Hour,
	},
	// SharingInviteType
	{
		Prefix: "sharing-invite",
		Limit:  10,
		Period: 1 * time.Hour,
	},
	// SharingPublicLink
	{
		Prefix: "sharing-public-link",
		Limit:  2000,
		Period: 1 * time.Hour,
	},
	// JobThumbnail
	{
		Prefix: "job-thumbnail",
		Limit:  5000,
		Period: 1 * time.Hour,
	},
	// JobShareTrack
	{
		Prefix: "job-share-track",
		Limit:  5000,
		Period: 1 * time.Hour,
	},
	// JobShareReplicate
	{
		Prefix: "job-share-replicate",
		Limit:  500,
		Period: 1 * time.Hour,
	},
	// JobShareUpload
	{
		Prefix: "job-share-upload",
		Limit:  500,
		Period: 1 * time.Hour,
	},
	// JobKonnector
	{
		Prefix: "job-konnector",
		Limit:  100,
		Period: 1 * time.Hour,
	},
	// JobZip
	{
		Prefix: "job-zip",
		Limit:  100,
		Period: 1 * time.Hour,
	},
	// JobSendMail
	{
		Prefix: "job-sendmail",
		Limit:  100,
		Period: 1 * time.Hour,
	},
	// JobService
	{
		Prefix: "job-service",
		Limit:  100,
		Period: 1 * time.Hour,
	},
	// JobNotification
	{
		Prefix: "job-push",
		Limit:  30,
		Period: 1 * time.Hour,
	},
	// SendHintByMail
	{
		Prefix: "send-hint",
		Limit:  2,
		Period: 1 * time.Hour,
	},
	// JobNotesPersistType
	{
		Prefix: "job-notes-persist",
		Limit:  100,
		Period: 1 * time.Hour,
	},
	// JobClientType
	{
		Prefix: "job-client",
		Limit:  100,
		Period: 1 * time.Hour,
	},
	// ExportType
	{
		Prefix: "export",
		Limit:  5,
		Period: 24 * time.Hour,
	},
	// WebhookTriggerType
	{
		Prefix: "webhook-trigger",
		Limit:  30,
		Period: 1 * time.Hour,
	},
	// JobCleanClientType
	{
		Prefix: "job-clean-clients",
		Limit:  100,
		Period: 1 * time.Hour,
	},
}

// Counter is an interface for counting number of attempts that can be used to
// rate limit the number of logins and 2FA tries, and thus block bruteforce
// attacks.
type Counter interface {
	Increment(key string, timeLimit time.Duration) (int64, error)
	Reset(key string) error
}

var globalCounter Counter
var globalCounterMu sync.Mutex
var counterCleanInterval = 1 * time.Second

func getCounter() Counter {
	globalCounterMu.Lock()
	defer globalCounterMu.Unlock()
	if globalCounter != nil {
		return globalCounter
	}
	client := config.GetConfig().RateLimitingStorage.Client()
	if client == nil {
		globalCounter = NewMemCounter()
	} else {
		globalCounter = NewRedisCounter(client)
	}
	return globalCounter
}

type memRef struct {
	val int64
	exp time.Time
}

type memCounter struct {
	mu   sync.Mutex
	vals map[string]*memRef
}

// NewMemCounter returns a in-memory counter.
func NewMemCounter() Counter {
	counter := &memCounter{vals: make(map[string]*memRef)}
	go counter.cleaner()
	return counter
}

func (c *memCounter) cleaner() {
	for range time.Tick(counterCleanInterval) {
		now := time.Now()
		for k, v := range c.vals {
			if now.After(v.exp) {
				delete(c.vals, k)
			}
		}
	}
}

func (c *memCounter) Increment(key string, timeLimit time.Duration) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, ok := c.vals[key]; !ok {
		c.vals[key] = &memRef{
			val: 0,
			exp: time.Now().Add(timeLimit),
		}
	}
	c.vals[key].val++
	return c.vals[key].val, nil
}

func (c *memCounter) Reset(key string) error {
	delete(c.vals, key)
	return nil
}

type redisCounter struct {
	Client redis.UniversalClient
	ctx    context.Context
}

// NewRedisCounter returns a counter that can be mutualized between several
// cozy-stack processes by using redis.
func NewRedisCounter(client redis.UniversalClient) Counter {
	return &redisCounter{client, context.Background()}
}

// incrWithTTL is a lua script for redis to increment a counter and sets a TTL
// if it doesn't have one.
const incrWithTTL = `
local n = redis.call("INCR", KEYS[1])
if redis.call("TTL", KEYS[1]) == -1 then
  redis.call("EXPIRE", KEYS[1], KEYS[2])
end
return n
`

func (r *redisCounter) Increment(key string, timeLimit time.Duration) (int64, error) {
	ttl := strconv.FormatInt(int64(timeLimit/time.Second), 10)
	count, err := r.Client.Eval(r.ctx, incrWithTTL, []string{key, ttl}).Result()
	if err != nil {
		return 0, err
	}
	return count.(int64), nil
}

func (r *redisCounter) Reset(key string) error {
	_, err := r.Client.Del(r.ctx, key).Result()
	return err
}

// CheckRateLimit returns an error if the counter for the given type and
// instance has reached the limit.
func CheckRateLimit(p prefixer.Prefixer, ct CounterType) error {
	return CheckRateLimitKey(p.DomainName(), ct)
}

// CheckRateLimitKey allows to check the rate-limit for a key
func CheckRateLimitKey(customKey string, ct CounterType) error {
	cfg := configs[ct]
	key := cfg.Prefix + ":" + customKey
	val, err := getCounter().Increment(key, cfg.Period)
	if err != nil {
		return err
	}
	// The first time we reach the limit, we provide a specific error message.
	// This allows to log a warning only once if needed.
	if val == cfg.Limit+1 {
		return ErrRateLimitReached
	}
	if val > cfg.Limit {
		return ErrRateLimitExceeded
	}
	return nil
}

// ResetCounter sets again to zero the counter for the given type and instance.
func ResetCounter(p prefixer.Prefixer, ct CounterType) {
	cfg := configs[ct]
	key := cfg.Prefix + ":" + p.DomainName()
	_ = getCounter().Reset(key)
}

// IsLimitReachedOrExceeded return true if the limit has been reached or
// exceeded, false otherwise.
func IsLimitReachedOrExceeded(err error) bool {
	return err == ErrRateLimitReached || err == ErrRateLimitExceeded
}

// GetMaximumLimit returns the limit of a CounterType
func GetMaximumLimit(ct CounterType) int64 {
	return configs[ct].Limit
}

// SetMaximumLimit sets a new limit for a CounterType
func SetMaximumLimit(ct CounterType, newLimit int64) {
	configs[ct].Limit = newLimit
}
