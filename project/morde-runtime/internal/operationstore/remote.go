package operationstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	RemoteNamespaceCore     = "runtime_core"
	RemoteNamespaceBrowser  = "browser"
	RemoteMaxEvents         = 65536
	RemoteMaxEventDataBytes = 8 << 20
	RemoteMaxLedgerBytes    = 128 << 20
	remotePageSize          = 128
	remoteRequestLimit      = RemoteMaxEventDataBytes + (64 << 10)
	remoteResponseLimit     = RemoteMaxLedgerBytes + (1 << 20)
)

type RemoteOptions struct {
	HostID     string
	Generation int64
	Namespace  string
	PortalURL  string
	Credential string
	Client     *http.Client
}

type Remote struct {
	mu         sync.Mutex
	options    RemoteOptions
	portal     *url.URL
	client     *http.Client
	events     []Event
	lastHash   string
	totalBytes int64
	poison     error
	closed     bool
	reconciled bool
}

type pageResponse struct {
	Items   []Event `json:"items"`
	HasMore bool    `json:"has_more"`
}

// OpenRemote connects to the authenticated Host Replica ledger and validates
// the complete remote chain before returning. It never falls back to local
// writes when Portal is unavailable.
func OpenRemote(options RemoteOptions) (*Remote, error) {
	if !validRemoteIdentifier(options.HostID) || options.Generation < 1 ||
		(options.Namespace != RemoteNamespaceCore && options.Namespace != RemoteNamespaceBrowser) ||
		len(options.Credential) != 43 || strings.ContainsAny(options.Credential, "\r\n") {
		return nil, errors.New("remote operation ledger identity is invalid")
	}
	parsed, err := url.Parse(options.PortalURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil ||
		parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("remote operation ledger requires a Portal HTTPS origin")
	}
	client := options.Client
	if client == nil {
		client = &http.Client{
			Timeout:       20 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	remote := &Remote{options: options, portal: parsed, client: client}
	if err := remote.reload(context.Background()); err != nil {
		return nil, fmt.Errorf("load remote operation ledger: %w", err)
	}
	return remote, nil
}

// Reconcile imports a missing legacy suffix after confirming that the local
// and Portal ledgers share the same hash-chain prefix. A Portal extension is
// accepted, while any fork fails startup.
func (r *Remote) Reconcile(legacy EventStore) error {
	if legacy == nil {
		return errors.New("legacy operation ledger is required")
	}
	localEvents, err := legacy.Events()
	if err != nil {
		return fmt.Errorf("read legacy operation ledger: %w", err)
	}
	if _, _, err := validateEvents(localEvents); err != nil {
		return fmt.Errorf("validate legacy operation ledger: %w", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.checkReady(); err != nil {
		return err
	}
	shared := len(localEvents)
	if len(r.events) < shared {
		shared = len(r.events)
	}
	for i := 0; i < shared; i++ {
		if !sameEvent(localEvents[i], r.events[i]) {
			r.poison = errors.New("legacy and Portal operation ledgers have diverged")
			return fmt.Errorf("%w: %v", ErrStoreUnhealthy, r.poison)
		}
	}
	for i := len(r.events); i < len(localEvents); i++ {
		if err := r.appendExactLocked(context.Background(), localEvents[i]); err != nil {
			r.poison = err
			return fmt.Errorf("import legacy operation event %d: %w", localEvents[i].Sequence, err)
		}
	}
	r.reconciled = true
	return nil
}

func (r *Remote) Events() ([]Event, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.checkReady(); err != nil {
		return nil, err
	}
	return cloneEvents(r.events), nil
}

// Append waits for the Portal's committed acknowledgement before advancing the
// in-memory head. An ambiguous network failure is resolved by re-reading the
// ledger; unresolved uncertainty poisons the store until process restart.
func (r *Remote) Append(kind string, data any) error {
	if kind == "" {
		return errors.New("journal event kind is required")
	}
	encodedData, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("encode journal event: %w", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.checkReady(); err != nil {
		return err
	}
	if !r.reconciled {
		return errors.New("remote operation ledger has not been reconciled with the legacy journal")
	}
	event, err := makeEvent(uint64(len(r.events)+1), kind, encodedData, r.lastHash)
	if err != nil {
		return err
	}
	return r.appendExactLocked(context.Background(), event)
}

func (r *Remote) Ready() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.closed && r.poison == nil && r.reconciled
}

func (r *Remote) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	r.options.Credential = ""
	return nil
}

func (r *Remote) appendExactLocked(ctx context.Context, event Event) error {
	if err := validateNextEvent(r.events, r.totalBytes, event); err != nil {
		return err
	}
	var acknowledged Event
	_, err := r.request(ctx, http.MethodPost, r.endpoint(), event, &acknowledged)
	if err == nil {
		if sameEvent(event, acknowledged) {
			r.add(event)
			return nil
		}
		err = errors.New("Portal returned an invalid operation ledger acknowledgement")
	}
	// A POST may commit even when its response is lost. Confirm the exact hash
	// before allowing the caller to proceed to a side effect.
	if reloadErr := r.reloadLocked(ctx); reloadErr != nil {
		r.poison = errors.Join(err, reloadErr)
		return fmt.Errorf("%w: operation ledger commit could not be confirmed", ErrStoreUnhealthy)
	}
	if int(event.Sequence) == len(r.events) && sameEvent(event, r.events[len(r.events)-1]) {
		return nil
	}
	r.poison = err
	return fmt.Errorf("%w: operation ledger append was not confirmed", ErrStoreUnhealthy)
}

func (r *Remote) add(event Event) {
	r.events = append(r.events, cloneEvent(event))
	r.lastHash = event.Hash
	r.totalBytes += int64(len(event.Data))
}

func (r *Remote) reload(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reloadLocked(ctx)
}

func (r *Remote) reloadLocked(ctx context.Context) error {
	var loaded []Event
	var after uint64
	var totalBytes int64
	for {
		target := r.endpoint() + "?after_sequence=" + strconv.FormatUint(after, 10) + "&limit=" + strconv.Itoa(remotePageSize)
		var page pageResponse
		if _, err := r.request(ctx, http.MethodGet, target, nil, &page); err != nil {
			return err
		}
		if len(page.Items) > remotePageSize || page.HasMore && len(page.Items) == 0 {
			return errors.New("Portal returned an invalid operation ledger page")
		}
		for _, event := range page.Items {
			if err := validateNextEvent(loaded, totalBytes, event); err != nil {
				return fmt.Errorf("Portal operation ledger integrity check failed: %w", err)
			}
			loaded = append(loaded, cloneEvent(event))
			totalBytes += int64(len(event.Data))
			after = event.Sequence
		}
		if !page.HasMore {
			break
		}
	}
	r.events = loaded
	r.lastHash = ""
	if len(loaded) > 0 {
		r.lastHash = loaded[len(loaded)-1].Hash
	}
	r.totalBytes = totalBytes
	return nil
}

func (r *Remote) request(ctx context.Context, method, target string, body any, output any) ([]byte, error) {
	var source io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, errors.New("encode remote operation ledger request")
		}
		if len(encoded) > remoteRequestLimit {
			return nil, errors.New("remote operation ledger request exceeds the size limit")
		}
		source = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, target, source)
	if err != nil {
		return nil, errors.New("create remote operation ledger request")
	}
	request.Header.Set("Authorization", "Bearer "+r.options.Credential)
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := r.client.Do(request)
	if err != nil {
		return nil, errors.New("Portal operation ledger request failed")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("Portal operation ledger request failed with status %d", response.StatusCode)
	}
	limit := int64(remoteResponseLimit)
	if output == nil {
		if _, err := io.Copy(io.Discard, io.LimitReader(response.Body, limit)); err != nil {
			return nil, errors.New("read remote operation ledger acknowledgement")
		}
		return nil, nil
	}
	encoded, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil || int64(len(encoded)) > limit {
		return nil, errors.New("remote operation ledger response exceeds the size limit")
	}
	if err := decodeOne(encoded, output); err != nil {
		return nil, errors.New("decode remote operation ledger response")
	}
	return encoded, nil
}

func (r *Remote) endpoint() string {
	return r.portal.JoinPath("api", "host-replicas", r.options.HostID, "generations",
		strconv.FormatInt(r.options.Generation, 10), "operation-ledger", r.options.Namespace).String()
}

func (r *Remote) checkReady() error {
	if r.closed {
		return ErrStoreClosed
	}
	if r.poison != nil {
		return fmt.Errorf("%w: %v", ErrStoreUnhealthy, r.poison)
	}
	return nil
}

func makeEvent(sequence uint64, kind string, data json.RawMessage, previousHash string) (Event, error) {
	input := hashInput{Version: journalVersion, Sequence: sequence, Kind: kind, Data: data, PreviousHash: previousHash}
	encoded, err := json.Marshal(input)
	if err != nil {
		return Event{}, fmt.Errorf("encode journal hash input: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return Event{Version: input.Version, Sequence: sequence, Kind: kind, Data: append(json.RawMessage(nil), data...),
		PreviousHash: previousHash, Hash: hex.EncodeToString(digest[:])}, nil
}

func validateEvents(events []Event) (string, int64, error) {
	var total int64
	for index, event := range events {
		if err := validateNextEvent(events[:index], total, event); err != nil {
			return "", total, err
		}
		total += int64(len(event.Data))
	}
	if len(events) == 0 {
		return "", 0, nil
	}
	return events[len(events)-1].Hash, total, nil
}

func validateNextEvent(events []Event, totalBytes int64, event Event) error {
	if len(events) >= RemoteMaxEvents || totalBytes+int64(len(event.Data)) > RemoteMaxLedgerBytes || len(event.Data) == 0 || len(event.Data) > RemoteMaxEventDataBytes {
		return errors.New("operation ledger capacity or event size limit exceeded")
	}
	if event.Version != journalVersion || event.Sequence != uint64(len(events)+1) || event.Kind == "" || len(event.Kind) > 128 ||
		!json.Valid(event.Data) || event.PreviousHash != previousEventHash(events) {
		return errors.New("operation ledger sequence or event fields are invalid")
	}
	expected, err := makeEvent(event.Sequence, event.Kind, event.Data, event.PreviousHash)
	if err != nil {
		return err
	}
	if expected.Hash != event.Hash {
		return errors.New("operation ledger event hash mismatch")
	}
	return nil
}

func previousEventHash(events []Event) string {
	if len(events) == 0 {
		return ""
	}
	return events[len(events)-1].Hash
}

func sameEvent(left, right Event) bool {
	return left.Version == right.Version && left.Sequence == right.Sequence && left.Kind == right.Kind &&
		bytes.Equal(left.Data, right.Data) && left.PreviousHash == right.PreviousHash && left.Hash == right.Hash
}

func cloneEvent(event Event) Event {
	event.Data = append(json.RawMessage(nil), event.Data...)
	return event
}

func cloneEvents(events []Event) []Event {
	cloned := make([]Event, len(events))
	for i := range events {
		cloned[i] = cloneEvent(events[i])
	}
	return cloned
}

func decodeOne(encoded []byte, output any) error {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func validRemoteIdentifier(value string) bool {
	if len(value) < 8 || len(value) > 64 {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}
