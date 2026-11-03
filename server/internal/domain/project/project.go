package project

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"strings"
	"unicode"
	"unicode/utf8"

	"morde/server/internal/domain"
	"morde/server/internal/domain/personal"
)

const idAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

type Role string

const (
	RoleOwner  Role = "owner"
	RoleMember Role = "member"
	RoleViewer Role = "viewer"
)

type Project struct {
	ID          string
	OwnerID     string
	Name        string
	Description string
	Version     int64
}

type Member struct {
	ProjectID string
	UserID    string
	Role      Role
	Version   int64
}

type Invitation struct {
	ID        string
	ProjectID string
	InviterID string
	InviteeID string
	Role      Role
	State     InvitationState
	Version   int64
}

type InvitationState string

const (
	InvitationPending  InvitationState = "pending"
	InvitationAccepted InvitationState = "accepted"
	InvitationRevoked  InvitationState = "revoked"
)

type ResourceKind string

const (
	ResourceHost      ResourceKind = "host"
	ResourceWorkspace ResourceKind = "workspace"
)

type Resource struct {
	ProjectID  string
	Kind       ResourceKind
	ResourceID string
	Version    int64
}

type ContentKind string

const (
	ContentMemory   ContentKind = "memory"
	ContentGoal     ContentKind = "goal"
	ContentDocument ContentKind = "document"
	ContentFeedPost ContentKind = "feed_post"
	ContentIdea     ContentKind = "idea"
	ContentLibrary  ContentKind = "library_item"
	ContentBrief    ContentKind = "brief"
)

type Content struct {
	ProjectID string
	Kind      ContentKind
	ID        string
	Revision  int64
	Body      []byte
	UpdatedBy string
}

// ContentComment is an annotation attached to one immutable content revision
// and row. Revision is the comment's own compare-and-swap version.
type ContentComment struct {
	ProjectID       string      `json:"project_id"`
	Kind            ContentKind `json:"kind"`
	ContentID       string      `json:"content_id"`
	ContentRevision int64       `json:"content_revision"`
	RowID           string      `json:"row_id"`
	ID              string      `json:"id"`
	Revision        int64       `json:"revision"`
	Body            string      `json:"body,omitempty"`
	Deleted         bool        `json:"deleted,omitempty"`
	UpdatedBy       string      `json:"updated_by"`
}

// ContentSearchResult contains a bounded excerpt from a current project content revision.
type ContentSearchResult struct {
	ProjectID string      `json:"project_id"`
	Kind      ContentKind `json:"kind"`
	ContentID string      `json:"content_id"`
	Revision  int64       `json:"revision"`
	Title     string      `json:"title"`
	Excerpt   string      `json:"excerpt"`
}

const (
	MaxContentSearchQueryRunes = 200
	MaxContentSearchResults    = 50
	MaxContentCommentBodyRunes = 500
	MaxContentCommentsPerRow   = 100
)

func NewID() (string, error) {
	const prefix = "j"
	var id [8]byte
	id[0] = prefix[0]
	max := big.NewInt(int64(len(idAlphabet)))
	for i := 1; i < len(id); i++ {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", fmt.Errorf("generate project id: %w", err)
		}
		id[i] = idAlphabet[n.Int64()]
	}
	return string(id[:]), nil
}

func ValidID(id string) bool {
	if len(id) != 8 || id[0] != 'j' {
		return false
	}
	for i := 1; i < len(id); i++ {
		if !strings.ContainsRune(idAlphabet, rune(id[i])) {
			return false
		}
	}
	return true
}

func New(id, ownerID, name, description string) (Project, error) {
	value := Project{ID: id, OwnerID: ownerID, Name: strings.TrimSpace(name), Description: strings.TrimSpace(description), Version: 1}
	if err := value.Validate(); err != nil {
		return Project{}, err
	}
	return value, nil
}

func (p Project) Validate() error {
	if !ValidID(p.ID) {
		return errors.New("project id is invalid")
	}
	if !domain.ValidUserID(p.OwnerID) {
		return errors.New("project owner id is invalid")
	}
	if p.Name == "" || strings.TrimSpace(p.Name) != p.Name || !utf8.ValidString(p.Name) || utf8.RuneCountInString(p.Name) > 120 {
		return errors.New("project name is invalid")
	}
	if strings.TrimSpace(p.Description) != p.Description || !utf8.ValidString(p.Description) || utf8.RuneCountInString(p.Description) > 2000 {
		return errors.New("project description is invalid")
	}
	if p.Version < 1 {
		return errors.New("project version is invalid")
	}
	return nil
}

func ValidRole(role Role) bool {
	return role == RoleOwner || role == RoleMember || role == RoleViewer
}

func ValidInvitationRole(role Role) bool {
	return role == RoleMember || role == RoleViewer
}

func RoleAtLeast(actual, minimum Role) bool {
	level := func(role Role) int {
		switch role {
		case RoleViewer:
			return 1
		case RoleMember:
			return 2
		case RoleOwner:
			return 3
		default:
			return 0
		}
	}
	return level(actual) > 0 && level(minimum) > 0 && level(actual) >= level(minimum)
}

func ValidInvitationState(state InvitationState) bool {
	return state == InvitationPending || state == InvitationAccepted || state == InvitationRevoked
}

func ValidResourceKind(kind ResourceKind) bool {
	return kind == ResourceHost || kind == ResourceWorkspace
}

func ValidContentKind(kind ContentKind) bool {
	return kind == ContentMemory || kind == ContentGoal || kind == ContentDocument ||
		kind == ContentFeedPost || kind == ContentIdea || kind == ContentLibrary || kind == ContentBrief
}

func ValidMuseContentKind(kind ContentKind) bool {
	return kind == ContentFeedPost || kind == ContentIdea || kind == ContentLibrary || kind == ContentBrief
}

func ValidContentID(id string) bool {
	if id == "" || id != strings.TrimSpace(id) || !utf8.ValidString(id) || utf8.RuneCountInString(id) > 128 {
		return false
	}
	for _, char := range id {
		if unicode.IsControl(char) {
			return false
		}
	}
	return true
}

func ValidateContentBody(kind ContentKind, id string, body []byte) error {
	if !ValidContentKind(kind) || !ValidContentID(id) {
		return domain.InvalidArgument("content", "project content is invalid")
	}
	if len(body) > personal.MaxMutationBodyBytes {
		return domain.NewError(domain.CodeRequestTooLarge, "project content exceeds the size limit")
	}
	if !utf8.Valid(body) {
		return invalidProjectContent()
	}
	switch kind {
	case ContentMemory:
		return personal.ValidateObjectBody(personal.ObjectTypeMemory, id, body)
	case ContentGoal:
		var fields map[string]json.RawMessage
		if json.Unmarshal(body, &fields) != nil || fields == nil || fields["related_material_ids"] != nil {
			return invalidProjectContent()
		}
		return personal.ValidateObjectBody(personal.ObjectTypeGoal, id, body)
	case ContentDocument:
		return validateProjectDocumentBody(body)
	case ContentBrief:
		return validateProjectDocumentBody(body)
	case ContentFeedPost:
		return personal.ValidateObjectBody(personal.ObjectTypeFeedPost, id, body)
	case ContentIdea:
		var value struct {
			Title   string   `json:"title"`
			Content string   `json:"content"`
			Tags    []string `json:"tags"`
		}
		if !decodeProjectContent(body, &value, "title", "content", "tags") ||
			!validProjectText(value.Title, 1, 200) || strings.TrimSpace(value.Title) != value.Title ||
			!validProjectText(value.Content, 1, 65536) || !validProjectTags(value.Tags) {
			return invalidProjectContent()
		}
		return nil
	case ContentLibrary:
		var value struct {
			Name      string  `json:"name"`
			Kind      string  `json:"kind"`
			MIME      *string `json:"mime"`
			SizeBytes *int64  `json:"size_bytes"`
		}
		if !decodeProjectContent(body, &value, "name", "kind", "mime", "size_bytes") ||
			!validProjectText(value.Name, 1, 255) || value.Name == "." || value.Name == ".." ||
			strings.ContainsAny(value.Name, "/\\") || hasProjectControl(value.Name) ||
			(value.Kind != "artifact" && value.Kind != "media") ||
			(value.MIME != nil && runeLengthProject(*value.MIME) > 128) ||
			(value.SizeBytes != nil && *value.SizeBytes < 0) {
			return invalidProjectContent()
		}
		return nil
	default:
		return invalidProjectContent()
	}
}

func validateProjectDocumentBody(body []byte) error {
	var value struct {
		Title   string   `json:"title"`
		Summary *string  `json:"summary"`
		Content string   `json:"content"`
		Tags    []string `json:"tags"`
	}
	if !decodeProjectContent(body, &value, "title", "summary", "content", "tags") ||
		!validProjectText(value.Title, 1, 200) || strings.TrimSpace(value.Title) != value.Title ||
		(value.Summary != nil && !validProjectText(*value.Summary, 0, 4096)) ||
		!validProjectText(value.Content, 1, 65536) || !validProjectTags(value.Tags) {
		return invalidProjectContent()
	}
	return nil
}

func validProjectTags(tags []string) bool {
	if len(tags) > 32 {
		return false
	}
	seen := make(map[string]struct{}, len(tags))
	for _, tag := range tags {
		if !validProjectText(tag, 1, 64) || strings.TrimSpace(tag) != tag {
			return false
		}
		if _, exists := seen[tag]; exists {
			return false
		}
		seen[tag] = struct{}{}
	}
	return true
}

func hasProjectControl(value string) bool {
	for _, char := range value {
		if unicode.IsControl(char) {
			return true
		}
	}
	return false
}

func runeLengthProject(value string) int { return utf8.RuneCountInString(value) }

func ValidateContentSearchQuery(query string) error {
	if !utf8.ValidString(query) || strings.TrimSpace(query) != query || runeLengthProject(query) < 1 ||
		runeLengthProject(query) > MaxContentSearchQueryRunes || hasProjectControl(query) {
		return domain.InvalidArgument("query", "project content search query is invalid")
	}
	return nil
}

func ValidateContentComment(value ContentComment) error {
	if !ValidID(value.ProjectID) || !ValidMuseContentKind(value.Kind) || !ValidContentID(value.ContentID) ||
		value.ContentRevision < 1 || !ValidContentID(value.RowID) || !ValidCommentID(value.ID) ||
		value.Revision < 1 || !domain.ValidUserID(value.UpdatedBy) ||
		(value.Deleted && value.Body != "") ||
		(!value.Deleted && (!utf8.ValidString(value.Body) || !validProjectText(value.Body, 1, MaxContentCommentBodyRunes) || strings.TrimSpace(value.Body) != value.Body)) {
		return domain.InvalidArgument("comment", "project content comment is invalid")
	}
	return nil
}

func ValidCommentID(id string) bool {
	if len(id) != 8 || id[0] != 'c' {
		return false
	}
	for i := 1; i < len(id); i++ {
		if !strings.ContainsRune(idAlphabet, rune(id[i])) {
			return false
		}
	}
	return true
}

func NewCommentID() (string, error) {
	const prefix = "c"
	var id [8]byte
	id[0] = prefix[0]
	max := big.NewInt(int64(len(idAlphabet)))
	for i := 1; i < len(id); i++ {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", fmt.Errorf("generate project comment id: %w", err)
		}
		id[i] = idAlphabet[n.Int64()]
	}
	return string(id[:]), nil
}

func ValidateContentSearchResult(value ContentSearchResult) error {
	if !ValidID(value.ProjectID) || !ValidMuseContentKind(value.Kind) || !ValidContentID(value.ContentID) ||
		value.Revision < 1 || runeLengthProject(value.Title) > 200 || runeLengthProject(value.Excerpt) > 240 ||
		!utf8.ValidString(value.Title) || !utf8.ValidString(value.Excerpt) {
		return domain.NewError(domain.CodeInternal, "project content search returned an invalid record")
	}
	return nil
}

func decodeProjectContent(body []byte, target any, allowedFields ...string) bool {
	var fields map[string]json.RawMessage
	if len(body) == 0 || json.Unmarshal(body, &fields) != nil || fields == nil {
		return false
	}
	allowed := make(map[string]struct{}, len(allowedFields))
	for _, field := range allowedFields {
		allowed[field] = struct{}{}
	}
	for field, raw := range fields {
		if _, ok := allowed[field]; !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return false
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if decoder.Decode(target) != nil {
		return false
	}
	return decoder.Decode(new(any)) == io.EOF
}

func validProjectText(value string, min, max int) bool {
	length := utf8.RuneCountInString(value)
	if length < min || length > max {
		return false
	}
	for _, char := range value {
		if unicode.IsControl(char) {
			return false
		}
	}
	return true
}

func invalidProjectContent() error {
	return domain.InvalidArgument("body", "project content body is invalid")
}
