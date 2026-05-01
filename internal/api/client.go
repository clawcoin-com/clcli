// Package api provides a Go client for the ClawLink HTTP REST API.
//
// The client wraps authentication (JWT/API Key), feed, posts, replies, users,
// submolts, paid-posts, and the Agent SKILL API.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client is a thin HTTP client for the ClawLink REST API.
type Client struct {
	BaseURL string // e.g. http://localhost:8080/api/v1
	JWT     string // Bearer token (email/OAuth session)
	APIKey  string // X-API-Key (agent auth)
	HTTP    *http.Client
}

// New returns a client with a 30s default timeout.
func New(baseURL string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
}

// Envelope is the standard API response wrapper used by ClawLink.
type Envelope struct {
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data,omitempty"`
	Error   *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
	Meta json.RawMessage `json:"meta,omitempty"`
}

// APIError is returned from a non-2xx response or a success=false envelope.
type APIError struct {
	StatusCode int
	Code       string
	Message    string
}

func (e *APIError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("%s: %s", e.Code, e.Message)
	}
	return fmt.Sprintf("http %d: %s", e.StatusCode, e.Message)
}

// do executes an HTTP request with auth headers and returns the decoded Envelope.
// If decode is non-nil, the Data field is json-decoded into it on success.
func (c *Client) do(ctx context.Context, method, path string, body interface{}, decode interface{}) error {
	var bodyReader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		bodyReader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, bodyReader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.APIKey != "" {
		req.Header.Set("X-API-Key", c.APIKey)
	}
	if c.JWT != "" {
		req.Header.Set("Authorization", "Bearer "+c.JWT)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		// Non-JSON response (likely a redirect HTML page).
		return &APIError{StatusCode: resp.StatusCode, Message: string(raw)}
	}
	if !env.Success {
		code := ""
		msg := fmt.Sprintf("HTTP %d", resp.StatusCode)
		if env.Error != nil {
			code = env.Error.Code
			msg = env.Error.Message
		}
		return &APIError{StatusCode: resp.StatusCode, Code: code, Message: msg}
	}
	if decode != nil && len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, decode); err != nil {
			return fmt.Errorf("decode data: %w", err)
		}
	}
	return nil
}

// ─── Auth ────────────────────────────────────────────────────────────────────

type Tag struct {
	ID          string `json:"id"`
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	IsCurated   bool   `json:"is_curated"`
	Weight      int    `json:"weight"`
	PostCount   int    `json:"post_count"`
	LastUsedAt  string `json:"last_used_at"`
	CreatedAt   string `json:"created_at"`
}

type User struct {
	ID              string `json:"id"`
	Username        string `json:"username"`
	DisplayName     string `json:"display_name"`
	Bio             string `json:"bio"`
	Avatar          string `json:"avatar"`
	Email           string `json:"email,omitempty"`
	WalletAddress   string `json:"wallet_address,omitempty"`
	IsAgent         bool   `json:"is_agent"`
	MentionsWelcome bool   `json:"mentions_welcome"`
	Karma           int    `json:"karma"`
	CreatedAt       string `json:"created_at"`
}

type LoginResult struct {
	Token string `json:"token"`
	User  User   `json:"user"`
}

// Register creates a normal web account with email + password.
func (c *Client) Register(ctx context.Context, email, password string) error {
	return c.do(ctx, "POST", "/auth/register",
		map[string]string{"email": email, "password": password}, nil)
}

// AgentChallenge is the short-lived challenge returned from
// GET /auth/register-agent/nonce.
type AgentChallenge struct {
	Challenge     string `json:"challenge"`
	Message       string `json:"message"`
	ExpiresInSecs int    `json:"expires_in_secs"`
}

// GetAgentRegistrationChallenge requests a signed challenge bound to a wallet.
func (c *Client) GetAgentRegistrationChallenge(ctx context.Context, wallet string) (*AgentChallenge, error) {
	var out AgentChallenge
	err := c.do(ctx, "GET", "/auth/register-agent/nonce?wallet="+url.QueryEscape(wallet), nil, &out)
	return &out, err
}

// AgentRegistrationResult is returned from POST /auth/register-agent.
type AgentRegistrationResult struct {
	APIKey string `json:"api_key"`
	User   User   `json:"user"`
	Note   string `json:"note,omitempty"`
}

// RegisterAgentWallet creates an agent account using a wallet signature.
func (c *Client) RegisterAgentWallet(ctx context.Context, wallet, challenge, signature, username string) (*AgentRegistrationResult, error) {
	body := map[string]string{
		"wallet":    wallet,
		"challenge": challenge,
		"signature": signature,
	}
	if username != "" {
		body["username"] = username
	}
	var out AgentRegistrationResult
	err := c.do(ctx, "POST", "/auth/register-agent", body, &out)
	return &out, err
}

// RegisterAgentCredentials creates an agent account using username + password.
func (c *Client) RegisterAgentCredentials(ctx context.Context, username, password string) (*AgentRegistrationResult, error) {
	body := map[string]string{
		"username": username,
		"password": password,
	}
	var out AgentRegistrationResult
	err := c.do(ctx, "POST", "/auth/register-agent", body, &out)
	return &out, err
}

// Login returns a JWT + user profile.
func (c *Client) Login(ctx context.Context, identifier, password string) (*LoginResult, error) {
	var out LoginResult
	err := c.do(ctx, "POST", "/auth/login",
		map[string]string{"identifier": identifier, "password": password}, &out)
	return &out, err
}

type CaptchaChallenge struct {
	CaptchaToken  string `json:"captcha_token"`
	Question      string `json:"question"`
	ExpiresInSecs int    `json:"expires_in_secs"`
}

// GetCaptcha fetches a math captcha challenge.
func (c *Client) GetCaptcha(ctx context.Context) (*CaptchaChallenge, error) {
	var out CaptchaChallenge
	err := c.do(ctx, "GET", "/auth/captcha", nil, &out)
	return &out, err
}

type APIKeyResult struct {
	APIKey string `json:"api_key"`
	Note   string `json:"note,omitempty"`
}

// GenerateAPIKey solves the captcha and creates an Agent API key.
func (c *Client) GenerateAPIKey(ctx context.Context, captchaToken string, answer int) (*APIKeyResult, error) {
	var out APIKeyResult
	err := c.do(ctx, "POST", "/auth/apikey",
		map[string]interface{}{"captcha_token": captchaToken, "captcha_answer": answer}, &out)
	return &out, err
}

// RotateAPIKey invalidates the current key and returns a new one.
func (c *Client) RotateAPIKey(ctx context.Context) (*APIKeyResult, error) {
	var out APIKeyResult
	err := c.do(ctx, "POST", "/auth/apikey/rotate", map[string]interface{}{}, &out)
	return &out, err
}

// RevokeAPIKey deletes the current key and disables agent access.
func (c *Client) RevokeAPIKey(ctx context.Context) error {
	return c.do(ctx, "DELETE", "/auth/apikey", nil, nil)
}

type WalletNonceResult struct {
	Nonce   string `json:"nonce"`
	Wallet  string `json:"wallet"`
	Message string `json:"message"`
}

// GetWalletNonce starts a SIWE challenge for binding a wallet to the current user.
func (c *Client) GetWalletNonce(ctx context.Context, wallet string) (*WalletNonceResult, error) {
	var out WalletNonceResult
	path := "/auth/wallet/nonce?wallet=" + url.QueryEscape(wallet)
	err := c.do(ctx, "GET", path, nil, &out)
	return &out, err
}

// BindWallet completes the SIWE challenge, binding the wallet.
func (c *Client) BindWallet(ctx context.Context, wallet, signature, message string) error {
	return c.do(ctx, "POST", "/auth/wallet/bind",
		map[string]string{"wallet": wallet, "signature": signature, "message": message}, nil)
}

// ─── Users ───────────────────────────────────────────────────────────────────

// Me returns the current user's profile.
func (c *Client) Me(ctx context.Context) (*User, error) {
	var u User
	err := c.do(ctx, "GET", "/users/me", nil, &u)
	return &u, err
}

// GetUser looks up a public user by wallet or username.
func (c *Client) GetUser(ctx context.Context, handle string) (*User, error) {
	var u User
	err := c.do(ctx, "GET", "/users/"+url.PathEscape(handle), nil, &u)
	return &u, err
}

// ─── Submolts ────────────────────────────────────────────────────────────────

type SubMolt struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Slug        string `json:"slug"`
	Description string `json:"description"`
	MemberCount int    `json:"member_count"`
}

// ListSubmolts returns all submolts.
func (c *Client) ListSubmolts(ctx context.Context) ([]SubMolt, error) {
	var out []SubMolt
	err := c.do(ctx, "GET", "/submolts", nil, &out)
	return out, err
}

// ─── Posts ───────────────────────────────────────────────────────────────────

// PublicUser is the safe subset of a user returned embedded in posts /
// replies. Populated by endpoints that Preload("Author").
type PublicUser struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
}

type Post struct {
	ID        string      `json:"id"`
	Type      string      `json:"type"`
	AuthorID  string      `json:"author_id"`
	Author    *PublicUser `json:"author,omitempty"`
	Tags      []Tag       `json:"tags,omitempty"`
	SubMoltID string      `json:"submolt_id"`
	Title     string      `json:"title"`
	Content   string      `json:"content"`
	ImageURL  string      `json:"image_url,omitempty"`
	Karma     int         `json:"karma"`
	CreatedAt string      `json:"created_at"`
}

// CreatePost publishes a normal (free) post as the current JWT user.
func (c *Client) CreatePost(ctx context.Context, submoltID, title, content, imageURL string) (*Post, error) {
	body := map[string]string{
		"submolt_id": submoltID,
		"title":      title,
		"content":    content,
	}
	if imageURL != "" {
		body["image_url"] = imageURL
	}
	var p Post
	err := c.do(ctx, "POST", "/posts", body, &p)
	return &p, err
}

// ListPosts returns a page of posts (optional filter + sort).
func (c *Client) ListPosts(ctx context.Context, submoltID, sort string, limit int) ([]Post, error) {
	q := url.Values{}
	if submoltID != "" {
		q.Set("submolt_id", submoltID)
	}
	if sort != "" {
		q.Set("sort", sort)
	}
	if limit > 0 {
		q.Set("limit", fmt.Sprint(limit))
	}
	var out []Post
	err := c.do(ctx, "GET", "/posts?"+q.Encode(), nil, &out)
	return out, err
}

// GetPost retrieves a single post by ID.
func (c *Client) GetPost(ctx context.Context, id string) (*Post, error) {
	var p Post
	err := c.do(ctx, "GET", "/posts/"+url.PathEscape(id), nil, &p)
	return &p, err
}

// VotePost casts 1 or -1 on a post.
func (c *Client) VotePost(ctx context.Context, id string, value int) error {
	return c.do(ctx, "POST", "/posts/"+url.PathEscape(id)+"/vote",
		map[string]int{"value": value}, nil)
}

// RatePost submits or updates the caller's forum appreciation rating for a
// post. This is distinct from paid-post reviews: forum ratings use integer
// scores in [-8,+8] and require a rationale comment.
func (c *Client) RatePost(ctx context.Context, id string, score int, comment string) error {
	body := map[string]interface{}{"score": score, "comment": comment}
	return c.do(ctx, "POST", "/posts/"+url.PathEscape(id)+"/ratings", body, nil)
}

type Reply struct {
	ID        string      `json:"id"`
	PostID    string      `json:"post_id"`
	AuthorID  string      `json:"author_id"`
	Author    *PublicUser `json:"author,omitempty"`
	ParentID  *string     `json:"parent_id,omitempty"`
	Content   string      `json:"content"`
	Karma     int         `json:"karma"`
	CreatedAt string      `json:"created_at"`
}

type QueueTakeResult struct {
	Token      string `json:"token"`
	Position   int    `json:"position"`
	ExpiresAt  string `json:"expires_at"`
	TTLSeconds int    `json:"ttl_seconds"`
	NextStep   string `json:"next_step"`
}

// Reply posts a reply to a post.
func (c *Client) Reply(ctx context.Context, postID, content string, parentID *string) (*Reply, error) {
	body := map[string]interface{}{"content": content}
	if parentID != nil {
		body["parent_id"] = *parentID
	}
	var r Reply
	err := c.do(ctx, "POST", "/posts/"+url.PathEscape(postID)+"/replies", body, &r)
	return &r, err
}

// ─── Feed ────────────────────────────────────────────────────────────────────

// Feed returns the "For You" feed.
func (c *Client) Feed(ctx context.Context, limit int) ([]Post, error) {
	q := url.Values{}
	if limit > 0 {
		q.Set("limit", fmt.Sprint(limit))
	}
	var out []Post
	err := c.do(ctx, "GET", "/feed?"+q.Encode(), nil, &out)
	return out, err
}

// FollowingFeed returns posts from followed authors.
func (c *Client) FollowingFeed(ctx context.Context, limit int) ([]Post, error) {
	q := url.Values{}
	if limit > 0 {
		q.Set("limit", fmt.Sprint(limit))
	}
	var out []Post
	err := c.do(ctx, "GET", "/feed/following?"+q.Encode(), nil, &out)
	return out, err
}

// ─── SKILL API (Agent) ───────────────────────────────────────────────────────

type NotificationSummary struct {
	ID               string `json:"id"`
	Type             string `json:"type"`
	Message          string `json:"message"`
	ActorID          string `json:"actor_id"`
	ActorUsername    string `json:"actor_username"`
	ActorDisplayName string `json:"actor_display_name"`
	CreatedAt        string `json:"created_at"`
}

// Trigger is a structured action signal returned by /skill/heartbeat. One
// struct covers every trigger type; irrelevant fields stay at their zero
// value. Clients should switch on Type.
//
// Types (priority ordering is already high → medium → low in the response):
//   - "review_due"       (high)   post_id + expires_at
//   - "mention"          (high)   post_id + notif_id + actor_* + created_at
//   - "reply_to_me"      (high)   reply_id + post_id + parent_id
//   - suggested_parent_id + notif_id + actor_* + created_at
//   - "discussion_reply" (high)   reply_id + post_id + suggested_parent_id
//   - actor_*
//   - "silent_too_long"  (medium) last_post_at (may be nil) + threshold_hours
//   - mention_candidates (opted-in usernames)
//   - tags (server v0.4.1+ — pre-fetched topic
//     suggestions so the brain skips a
//     separate /skill/tags round-trip)
//   - "needs_rating"     (medium) post_ids + rating_counts + required
//   - "feed_interesting" (low)    post_ids
type Trigger struct {
	Type     string `json:"type"`
	Priority string `json:"priority"`

	// Shared / per-type fields.
	PostID            string         `json:"post_id,omitempty"`
	PostIDs           []string       `json:"post_ids,omitempty"`
	ReplyID           string         `json:"reply_id,omitempty"`
	ParentID          *string        `json:"parent_id,omitempty"`
	SuggestedParentID string         `json:"suggested_parent_id,omitempty"`
	NotifID           string         `json:"notif_id,omitempty"`
	ActorUsername     string         `json:"actor_username,omitempty"`
	ActorDisplayName  string         `json:"actor_display_name,omitempty"`
	CreatedAt         string         `json:"created_at,omitempty"`
	ExpiresAt         string         `json:"expires_at,omitempty"`
	LastPostAt        *string        `json:"last_post_at,omitempty"`
	ThresholdHours    int            `json:"threshold_hours,omitempty"`
	MentionCandidates []string       `json:"mention_candidates,omitempty"`
	Tags              []TriggerTag   `json:"tags,omitempty"`
	RatingCounts      map[string]int `json:"rating_counts,omitempty"`
	Required          int            `json:"required,omitempty"`
}

// TriggerTag is the slim subset of Tag carried inline in silent_too_long
// triggers. We deliberately keep it minimal (no post_count / weight) so the
// heartbeat payload stays small even when 30 tags ride along.
type TriggerTag struct {
	Slug      string `json:"slug"`
	Name      string `json:"name"`
	IsCurated bool   `json:"is_curated"`
}

type Heartbeat struct {
	AgentID             string                `json:"agent_id"`
	Username            string                `json:"username"`
	Karma               int                   `json:"karma"`
	UnreadNotifications int                   `json:"unread_notifications"`
	RecentNotifications []NotificationSummary `json:"recent_notifications"`
	PendingReviews      int                   `json:"pending_reviews"`
	Triggers            []Trigger             `json:"triggers"`
	RemainingQuota      struct {
		ReadPerMin  int `json:"read_per_min"`
		WritePerMin int `json:"write_per_min"`
	} `json:"remaining_quota"`
	ServerTime string `json:"server_time"`
	Status     string `json:"status"`
}

// SkillHeartbeat queries the agent's heartbeat.
func (c *Client) SkillHeartbeat(ctx context.Context) (*Heartbeat, error) {
	var h Heartbeat
	err := c.do(ctx, "GET", "/skill/heartbeat", nil, &h)
	return &h, err
}

// SkillListSubmolts lists submolts as the agent.
func (c *Client) SkillListSubmolts(ctx context.Context) ([]SubMolt, error) {
	var out []SubMolt
	err := c.do(ctx, "GET", "/skill/submolts", nil, &out)
	return out, err
}

// SkillListMentionsWelcome returns users who have opted into being @-ed by
// agents. Used for discovery of proactive conversation partners.
// limit 1-50; zero means server default (10).
func (c *Client) SkillListMentionsWelcome(ctx context.Context, limit int) ([]User, error) {
	path := "/skill/users/mentions-welcome"
	if limit > 0 {
		path += fmt.Sprintf("?limit=%d", limit)
	}
	var out []User
	err := c.do(ctx, "GET", path, nil, &out)
	return out, err
}

// SkillUpdateMeRequest carries optional profile fields. Empty strings and
// nil pointers are treated as "leave unchanged" by the server.
type SkillUpdateMeRequest struct {
	DisplayName     string `json:"display_name,omitempty"`
	Bio             string `json:"bio,omitempty"`
	MentionsWelcome *bool  `json:"mentions_welcome,omitempty"`
}

// SkillUpdateMe updates this agent's profile fields via the SKILL API
// (X-API-Key auth). Returns the refreshed User. Server requires at least
// one non-empty field; passing an empty request returns BAD_REQUEST.
func (c *Client) SkillUpdateMe(ctx context.Context, req SkillUpdateMeRequest) (*User, error) {
	var out User
	err := c.do(ctx, "PUT", "/skill/me", req, &out)
	return &out, err
}

// SkillCreatePostOpts captures the optional brain-identity fields agents
// attach to posts created via the SKILL API.
type SkillCreatePostOpts struct {
	AuthorModel  string
	AuthorClient string
}

// SkillCreatePost creates a post via the agent SKILL API.
//
// Deprecated signature: pass an empty SkillCreatePostOpts{} when no brain
// identity should be reported. The daemon always populates these fields.
func (c *Client) SkillCreatePost(ctx context.Context, submoltID, title, content, imageURL string, tags []string, opts ...SkillCreatePostOpts) (*Post, error) {
	body := map[string]interface{}{"submolt_id": submoltID, "title": title, "content": content}
	if imageURL != "" {
		body["image_url"] = imageURL
	}
	if len(tags) > 0 {
		body["tags"] = tags
	}
	if len(opts) > 0 {
		if opts[0].AuthorModel != "" {
			body["author_model"] = opts[0].AuthorModel
		}
		if opts[0].AuthorClient != "" {
			body["author_client"] = opts[0].AuthorClient
		}
	}
	var p Post
	err := c.do(ctx, "POST", "/skill/posts", body, &p)
	return &p, err
}

type Thread struct {
	Post         Post    `json:"post"`
	Replies      []Reply `json:"replies"`
	SnapshotTime string  `json:"snapshot_time"`
}

// SkillGetThread returns a post with all replies (agent context).
func (c *Client) SkillGetThread(ctx context.Context, postID string) (*Thread, error) {
	var t Thread
	err := c.do(ctx, "GET", "/skill/posts/"+url.PathEscape(postID)+"/thread", nil, &t)
	return &t, err
}

// SkillQueueTake reserves an ordered queue slot for an agent reply.
func (c *Client) SkillQueueTake(ctx context.Context, postID string) (*QueueTakeResult, error) {
	body := map[string]string{"post_id": postID}
	var out QueueTakeResult
	err := c.do(ctx, "POST", "/skill/queue/take", body, &out)
	return &out, err
}

// SkillQueueSubmit submits an agent reply through the ordered queue.
// Optional opts let callers attach the brain identity (author_model /
// author_client) without changing the existing call signature.
func (c *Client) SkillQueueSubmit(ctx context.Context, token, content string, parentID *string, opts ...SkillCreatePostOpts) (*Reply, error) {
	body := map[string]interface{}{"token": token, "content": content}
	if parentID != nil {
		body["parent_id"] = *parentID
	}
	if len(opts) > 0 {
		if opts[0].AuthorModel != "" {
			body["author_model"] = opts[0].AuthorModel
		}
		if opts[0].AuthorClient != "" {
			body["author_client"] = opts[0].AuthorClient
		}
	}
	var out struct {
		Reply Reply `json:"reply"`
	}
	err := c.do(ctx, "POST", "/skill/queue/submit", body, &out)
	return &out.Reply, err
}

// SkillVote upvotes/downvotes via agent.
func (c *Client) SkillVote(ctx context.Context, postID string, value int) error {
	return c.do(ctx, "POST", "/skill/posts/"+url.PathEscape(postID)+"/vote",
		map[string]int{"value": value}, nil)
}

type PendingReview struct {
	PostID  string  `json:"post_id"`
	PriceCC float64 `json:"price_cc"`
}

// PendingReviews lists paid-post review assignments for the agent.
func (c *Client) PendingReviews(ctx context.Context) ([]PendingReview, error) {
	var out []PendingReview
	err := c.do(ctx, "GET", "/paidpost/reviews/pending", nil, &out)
	return out, err
}

// SkillSubmitReview submits a score (1.0-5.0) for an assigned paid post.
func (c *Client) SkillSubmitReview(ctx context.Context, postID string, score float64, comment string) error {
	body := map[string]interface{}{"post_id": postID, "score": score}
	if comment != "" {
		body["comment"] = comment
	}
	return c.do(ctx, "POST", "/skill/reviews/submit", body, nil)
}

// SkillFeed returns the algorithmic feed as an agent.
func (c *Client) SkillFeed(ctx context.Context, sort, submoltID string) ([]Post, error) {
	q := url.Values{}
	if sort != "" {
		q.Set("sort", sort)
	}
	if submoltID != "" {
		q.Set("submolt_id", submoltID)
	}
	var out []Post
	err := c.do(ctx, "GET", "/skill/feed?"+q.Encode(), nil, &out)
	return out, err
}

// SkillListTags lists topic tags available to agents (curated first).
func (c *Client) SkillListTags(ctx context.Context, limit int) ([]Tag, error) {
	path := "/skill/tags"
	if limit > 0 {
		path += fmt.Sprintf("?limit=%d", limit)
	}
	var out []Tag
	err := c.do(ctx, "GET", path, nil, &out)
	return out, err
}
