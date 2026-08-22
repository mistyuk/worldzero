package api

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"github.com/mistyuk/worldzero/internal/kernel/ids"
	"github.com/mistyuk/worldzero/internal/kernel/werr"
	"github.com/mistyuk/worldzero/internal/messaging"
)

// getInbox returns a citizen's direct messages, newest first.
//
// Cursor-paginated on `before` rather than an offset: offsets shift as new mail
// arrives, so a paging agent would see duplicates and gaps.
func (d Deps) getInbox(c *gin.Context) {
	p := MustPrincipal(c)

	before := c.Query("before")
	if before != "" && !ids.Valid(before, ids.Message) {
		fail(c, d.Logger, werr.New(werr.InvalidParams, "before must be a message id"))
		return
	}
	limit, err := int64Query(c, "limit", 50)
	if err != nil {
		fail(c, d.Logger, err)
		return
	}

	msgs, err := messaging.Inbox(c.Request.Context(), d.DB.Pool(), p.AgentID, before, int(limit))
	if err != nil {
		fail(c, d.Logger, err)
		return
	}
	unread, err := messaging.UnreadCount(c.Request.Context(), d.DB.Pool(), p.AgentID)
	if err != nil {
		fail(c, d.Logger, err)
		return
	}

	var next string
	if n := len(msgs); n > 0 {
		next = msgs[n-1].ID
	}

	c.JSON(http.StatusOK, gin.H{
		"messages":    msgs,
		"unread":      unread,
		"next_before": next,
	})
}

type markReadRequest struct {
	UpToID string `json:"up_to_id"`
}

// markRead acknowledges mail.
//
// Deliberately NOT an action verb. It changes nothing another citizen can
// observe, emits no event, and costs the world nothing — making it an action
// would spend an agent's physics budget on its own bookkeeping and put a row in
// the idempotency ledger for every inbox poll.
func (d Deps) markRead(c *gin.Context) {
	p := MustPrincipal(c)

	var req markReadRequest
	if err := decodeJSON(c, &req); err != nil {
		fail(c, d.Logger, err)
		return
	}
	if !ids.Valid(req.UpToID, ids.Message) {
		fail(c, d.Logger, werr.New(werr.InvalidParams, "up_to_id must be a message id"))
		return
	}

	var n int
	err := d.DB.Tx(c.Request.Context(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		n, err = messaging.MarkRead(ctx, tx, p.AgentID, req.UpToID, d.Clock.Now())
		return err
	})
	if err != nil {
		fail(c, d.Logger, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"marked_read": n})
}

// getRoom is what has been said in a place.
//
// Readable by anyone, including someone who was not present. A room is a public
// place; a world where you can only know what you personally witnessed has no
// gossip, no journalism and no history.
func (d Deps) getRoom(c *gin.Context) {
	limit, err := int64Query(c, "limit", 30)
	if err != nil {
		fail(c, d.Logger, err)
		return
	}
	id := c.Param("id")
	if !ids.Valid(id, ids.Location) {
		fail(c, d.Logger, werr.New(werr.NotFound, "no such location"))
		return
	}

	msgs, err := messaging.Overheard(c.Request.Context(), d.DB.Pool(), id, int(limit))
	if err != nil {
		fail(c, d.Logger, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"said": msgs})
}
