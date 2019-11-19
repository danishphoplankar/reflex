package rsql

import (
	"context"
	"database/sql"
	"strconv"
	"sync"
	"time"

	"github.com/luno/jettison/errors"
	"github.com/luno/reflex"
)

const (
	defaultStreamBackoff = time.Second * 10
)

// NewEventsTable returns a new events table.
func NewEventsTable(name string, opts ...EventsOption) *EventsTable {
	table := &EventsTable{
		schema: etableSchema{
			name:           name,
			timeField:      defaultEventTimeField,
			typeField:      defaultEventTypeField,
			foreignIDField: defaultEventForeignIDField,
			metadataField:  defaultMetadataField,
		},
		options: options{
			notifier: &stubNotifier{},
			backoff:  defaultStreamBackoff,
		},
	}
	for _, o := range opts {
		o(table)
	}

	table.gapCh = make(chan Gap)
	table.currentLoader = buildLoader(table.baseLoader, table.gapCh, table.enableCache, table.schema)

	eventsGapListenGauge.WithLabelValues(table.schema.name) // Init zero gap filling gauge.

	return table
}

// EventsOption defines a functional option to configure new event tables.
type EventsOption func(*EventsTable)

// WithEventTimeField provides an option to set the event DB timestamp field.
// It defaults to 'timestamp'.
func WithEventTimeField(field string) EventsOption {
	return func(table *EventsTable) {
		table.schema.timeField = field
	}
}

// WithEventTypeField provides an option to set the event DB type field.
// It defaults to 'type'.
func WithEventTypeField(field string) EventsOption {
	return func(table *EventsTable) {
		table.schema.typeField = field
	}
}

// WithEventForeignIDField provides an option to set the event DB foreignID field.
// It defaults to 'foreign_id'.
func WithEventForeignIDField(field string) EventsOption {
	return func(table *EventsTable) {
		table.schema.foreignIDField = field
	}
}

// WithEventMetadataField provides an option to set the event DB metadata field.
// It is disabled by default; ie. ''.
func WithEventMetadataField(field string) EventsOption {
	return func(table *EventsTable) {
		table.schema.metadataField = field
	}
}

// WithEventsNotifier provides an option to receive event notifications
// and trigger StreamClients when new events are available.
func WithEventsNotifier(notifier EventsNotifier) EventsOption {
	return func(table *EventsTable) {
		table.notifier = notifier
	}
}

// WithEventsInMemNotifier provides an option that enables an in-memory
// notifier.
//
// Note: This can have a significant impact on database load
// if the cache is disabled since all consumers might query
// the database on every event.
func WithEventsInMemNotifier() EventsOption {
	return func(table *EventsTable) {
		table.notifier = &inmemNotifier{}
	}
}

// WithEventsCacheEnabled provides an option to enable the read-through
// cache on the events tables.
// TODO(corver): Enable this by default.
func WithEventsCacheEnabled() EventsOption {
	return func(table *EventsTable) {
		table.enableCache = true
	}
}

// WithEventsBackoff provides an option to set the backoff period between polling
// the DB for new events. It defaults to 10s.
func WithEventsBackoff(d time.Duration) EventsOption {
	return func(table *EventsTable) {
		table.backoff = d
	}
}

// WithEventsLoader provides an option to set the base event loader.
// The default loader is configured with the WithEventsXField options.
func WithEventsLoader(loader Loader) EventsOption {
	return func(table *EventsTable) {
		table.baseLoader = loader
	}
}

// EventsTable provides reflex event insertion and streaming
// for a sql db table.
type EventsTable struct {
	options
	schema      etableSchema
	enableCache bool
	baseLoader  Loader

	// Stateful fields not cloned
	currentLoader Loader
	gapCh         chan Gap
	gapFns        []func(Gap)
	gapMu         sync.Mutex
}

// Insert inserts an event into the EventsTable and returns a function that
// can be optionally called to notify the table's EventNotifier of the change.
// The intended pattern for this function is:
//
//       notify, err := etable.Insert(ctx, tx, ...)
//       if err != nil {
//         return err
//       }
//	     defer notify()
//       return doWorkAndCommit(tx)
func (t *EventsTable) Insert(ctx context.Context, tx *sql.Tx, foreignID string,
	typ reflex.EventType) (NotifyFunc, error) {
	return t.InsertWithMetadata(ctx, tx, foreignID, typ, nil)
}

// InsertWithMetadata inserts an event with metadata into the EventsTable.
// Note metadata is disabled by default, enable with WithEventMetadataField option.
func (t *EventsTable) InsertWithMetadata(ctx context.Context, tx *sql.Tx, foreignID string,
	typ reflex.EventType, metadata []byte) (NotifyFunc, error) {
	if isNoop(foreignID, typ) {
		return nil, errors.New("inserting invalid noop event")
	}
	err := insertEvent(ctx, tx, t.schema, foreignID, typ, metadata)
	if err != nil {
		return noopFunc, err
	}

	return t.notifier.Notify, nil
}

// Clone returns a new etable cloned from the config of t with the new options applied.
// Note that the stateful fields are not clone, so the cache is not shared.
func (t *EventsTable) Clone(opts ...EventsOption) *EventsTable {
	table := &EventsTable{
		options:     t.options,
		schema:      t.schema,
		enableCache: t.enableCache,
		baseLoader:  nil,
	}
	for _, opt := range opts {
		opt(table)
	}

	table.gapCh = make(chan Gap)
	table.currentLoader = buildLoader(table.baseLoader, table.gapCh,
		table.enableCache, table.schema)

	return table
}

// Stream returns a StreamClient that streams events from the db.
// It is only safe for a single goroutine to use.
func (t *EventsTable) Stream(ctx context.Context, dbc *sql.DB, after string,
	opts ...reflex.StreamOption) reflex.StreamClient {

	sc := &streamclient{
		schema:  t.schema,
		after:   after,
		dbc:     dbc,
		ctx:     ctx,
		options: t.options,
		loader:  t.currentLoader,
	}

	for _, o := range opts {
		o(&sc.StreamOptions)
	}

	return sc
}

// ToStream returns a reflex StreamFunc interface of this EventsTable.
func (t *EventsTable) ToStream(dbc *sql.DB, opts1 ...reflex.StreamOption) reflex.StreamFunc {
	return func(ctx context.Context, after string,
		opts2 ...reflex.StreamOption) (client reflex.StreamClient, e error) {
		return t.Stream(ctx, dbc, after, append(opts1, opts2...)...), nil
	}
}

// ListenGaps adds f to a slice of functions that are called when a gap is detected.
// One first call, it starts a goroutine that serves these functions.
func (t *EventsTable) ListenGaps(f func(Gap)) {
	t.gapMu.Lock()
	defer t.gapMu.Unlock()
	if len(t.gapFns) == 0 {
		// Start serving gaps.
		eventsGapListenGauge.WithLabelValues(t.schema.name).Set(1)
		go func() {
			for gap := range t.gapCh {
				t.gapMu.Lock()
				for _, f := range t.gapFns {
					f(gap)
				}
				t.gapMu.Unlock()
			}
		}()
	}
	t.gapFns = append(t.gapFns, f)
}

// buildLoader returns a new layered event loader.
func buildLoader(baseLoader Loader, ch chan<- Gap, enableCache bool, schema etableSchema) Loader {
	if baseLoader == nil {
		baseLoader = makeBaseLoader(schema)
	}
	loader := wrapGapDetector(baseLoader, ch, schema.name)
	if enableCache {
		loader = newRCache(loader, schema.name).Load
	}
	return wrapNoopFilter(loader)
}

// options define config/state defined in EventsTable used by the streamclients.
type options struct {
	reflex.StreamOptions

	notifier EventsNotifier
	backoff  time.Duration
}

// etableSchema defines the mysql schema of an events table.
type etableSchema struct {
	name           string
	timeField      string
	typeField      string
	foreignIDField string
	metadataField  string
}

type streamclient struct {
	options

	schema etableSchema
	after  string
	prev   int64 // Previous (current) cursor.
	buf    []*reflex.Event
	dbc    *sql.DB
	ctx    context.Context

	// loader queries next events from the DB.
	loader Loader
}

// Recv blocks and returns the next event in the stream. It queries the db
// in batches buffering the results. If the buffer is not empty is pops one
// event and returns it. When querying and no new events are found it backs off
// before retrying. It blocks until it can return a non-nil event or an error.
// It is only safe for a single goroutine to call Recv.
func (s *streamclient) Recv() (*reflex.Event, error) {
	if err := s.ctx.Err(); err != nil {
		return nil, err
	}

	// Initialise cursor s.LastID once.
	var err error
	if s.StreamFromHead {
		s.prev, err = getLatestID(s.ctx, s.dbc, s.schema)
		if err != nil {
			return nil, err
		}
		s.StreamFromHead = false
	} else if s.after != "" {
		s.prev, err = strconv.ParseInt(s.after, 10, 64)
		if err != nil {
			return nil, ErrInvalidIntID
		}
		s.after = ""
	}

	for len(s.buf) == 0 {
		eventsPollCounter.WithLabelValues(s.schema.name).Inc()
		el, next, err := s.loader(s.ctx, s.dbc, s.prev, s.Lag)
		if err != nil {
			return nil, err
		}

		s.prev = next
		s.buf = el

		if len(el) > 0 {
			break
		}

		if err := s.wait(s.backoff); err != nil {
			return nil, err
		}
	}
	e := s.buf[0]
	s.prev = e.IDInt()
	s.buf = s.buf[1:]
	return e, nil
}

func (s *streamclient) wait(d time.Duration) error {
	if d == 0 {
		return nil
	}
	t := time.NewTimer(d)
	select {
	case <-s.notifier.C():
		return nil
	case <-t.C:
		return nil
	case <-s.ctx.Done():
		return s.ctx.Err()
	}
}

// isNoopEvent returns true if an event has "0" foreignID and 0 type.
func isNoopEvent(e *reflex.Event) bool {
	return isNoop(e.ForeignID, e.Type)
}

// isNoop returns true if the foreignID is "0" and the type 0.
func isNoop(foreignID string, typ reflex.EventType) bool {
	return foreignID == "0" && typ.ReflexType() == 0
}

// NotifyFunc notifies an events table's underlying EventsNotifier.
type NotifyFunc func()

var noopFunc NotifyFunc = func() {}

// stubNotifier is an implementation of EventsNotifier that does nothing.
type stubNotifier struct {
	c chan struct{}
}

func (m *stubNotifier) Notify() {
}

func (m *stubNotifier) C() <-chan struct{} {
	return m.c
}

// inmemNotifier is an in-memory implementation of EventsNotifier.
type inmemNotifier struct {
	mu        sync.Mutex
	listeners []chan struct{}
}

func (n *inmemNotifier) Notify() {
	n.mu.Lock()
	defer n.mu.Unlock()

	for _, l := range n.listeners {
		select {
		case l <- struct{}{}:
		default:
		}
	}
	n.listeners = nil
}

func (n *inmemNotifier) C() <-chan struct{} {
	n.mu.Lock()
	defer n.mu.Unlock()

	ch := make(chan struct{}, 1)
	n.listeners = append(n.listeners, ch)
	return ch
}

// EventsNotifier provides a way to receive notifications when an event is
// inserted in an EventsTable, and a way to trigger an EventsTable's
// StreamClients when there are new events available.
type EventsNotifier interface {
	// StreamWatcher is passed as the default StreamWatcher every time Stream()
	// is called on the EventsTable.
	StreamWatcher

	// Notify is called by reflex every time an event is inserted into the
	// EventsTable.
	Notify()
}

// StreamWatcher provides the ability to trigger the streamer when new events are available.
type StreamWatcher interface {
	// C returns a channel that blocks until the next event is available in the
	// StreamWatcher's EventsTable. C will be called every time a StreamClient
	// needs to wait for events.
	C() <-chan struct{}
}
