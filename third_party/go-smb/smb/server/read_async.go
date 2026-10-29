// Copyright (c) 2026 torrentfs-go contributors. MIT licensed; see LICENSE.
package server

import (
	"context"
	"encoding/binary"
	"fmt"

	"github.com/jfjallid/go-smb/smb"
)

// A read-containing frame keeps compound order without occupying the receive
// loop. Child contexts keep CANCEL scoped to one operation, even in a compound.
type readJob struct {
	ctx           context.Context
	cancel        context.CancelFunc
	done          chan struct{}
	ids           []uint64
	session       uint64
	tree          uint32
	handle        uint64
	currentID     uint64
	active        bool
	requestCancel context.CancelFunc
	requestDone   chan struct{}
	canceled      map[uint64]bool
}

func (c *Conn) queueReadFrame(raw []byte, ctx pduCtx) (bool, error) {
	if !c.Server.Config.ConcurrentReads || ctx.readJob != nil {
		return false, nil
	}
	var first smb.Header
	if len(raw) < 64 || first.UnmarshalBinary(raw[:64]) != nil {
		return false, nil
	}
	sess := c.session(first.SessionID)
	if sess == nil || !sess.Authenticated || sess.closing.Load() {
		return false, nil
	}
	var headers []smb.Header
	ids := make(map[uint64]bool)
	hasRead, control := false, false
	for rest := raw; len(rest) != 0; {
		if len(rest) < 64 || string(rest[:4]) != smb.ProtocolSmb2 {
			return true, fmt.Errorf("invalid compound header")
		}
		var h smb.Header
		if err := h.UnmarshalBinary(rest[:64]); err != nil {
			return true, err
		}
		if h.Flags&smb.SMB2_FLAGS_RELATED_OPERATIONS != 0 {
			if len(headers) == 0 {
				return true, fmt.Errorf("first compound operation cannot be related")
			}
		} else if h.SessionID != first.SessionID {
			return true, fmt.Errorf("compound operations must use one session")
		}
		if ctx.encrypted && ctx.authSessionID != first.SessionID {
			return true, fmt.Errorf("encrypted session identity mismatch")
		}
		hasRead = hasRead || h.Command == smb.CommandRead
		control = control || h.Command == smb.CommandNegotiate || h.Command == smb.CommandSessionSetup || h.Command == smb.CommandCancel
		headers = append(headers, h)
		if len(headers) > maxAsyncOps {
			return true, fmt.Errorf("too many compounded requests")
		}
		if ids[h.MessageID] {
			return true, fmt.Errorf("duplicate compound MessageID")
		}
		ids[h.MessageID] = true
		next := int(binary.LittleEndian.Uint32(rest[20:24]))
		if next == 0 {
			break
		}
		if next < 64 || next > len(rest) || next%8 != 0 {
			return true, fmt.Errorf("invalid compound offset")
		}
		rest = rest[next:]
	}
	if !hasRead {
		return false, nil
	}
	if control {
		return true, fmt.Errorf("negotiation and CANCEL cannot be compounded with READ")
	}
	readCtx, cancel := context.WithCancel(context.Background())
	job := &readJob{ctx: readCtx, cancel: cancel, done: make(chan struct{}), session: first.SessionID,
		tree: first.TreeID, canceled: make(map[uint64]bool)}
	for _, h := range headers {
		job.ids = append(job.ids, h.MessageID)
	}
	c.asyncMu.Lock()
	for _, id := range job.ids {
		if c.readJobs[id] != nil || c.asyncOps[id] != nil || id == ^uint64(0) {
			c.asyncMu.Unlock()
			cancel()
			return true, fmt.Errorf("duplicate or invalid outstanding MessageID")
		}
	}
	// Claim sequence numbers before dispatch so synchronous requests cannot
	// overtake a queued READ and reuse its MessageID.
	for _, id := range job.ids {
		if c.seenMsgIDs.seen(id) {
			c.asyncMu.Unlock()
			cancel()
			return true, fmt.Errorf("duplicate MessageID")
		}
	}
	closed := c.readStopping || sess.closing.Load()
	full := len(c.readJobs)+len(c.asyncOps)+len(job.ids) > maxAsyncOps
	if closed || full {
		c.asyncMu.Unlock()
		cancel()
		if err := verifyReadFrame(raw, ctx, sess); err != nil {
			return true, err
		}
		status := uint32(0xc000009a) // STATUS_INSUFFICIENT_RESOURCES
		if closed {
			status = smb.StatusUserSessionDeleted
		}
		return true, c.rejectReadFrame(ctx, headers, status)
	}
	if c.readJobs == nil {
		c.readJobs = make(map[uint64]*readJob)
	}
	for _, id := range job.ids {
		c.readJobs[id] = job
	}
	c.readWorkers.Add(1)
	c.asyncMu.Unlock()
	ctx.readCtx, ctx.readJob = readCtx, job
	go func() {
		defer c.readWorkers.Done()
		defer cancel()
		defer func() {
			c.asyncMu.Lock()
			for _, id := range job.ids {
				delete(c.readJobs, id)
			}
			c.asyncMu.Unlock()
			close(job.done)
			if recovered := recover(); recovered != nil {
				c.logger().Errorf("panic in read request: %v", recovered)
				c.close()
			}
		}()
		if err := c.dispatchSMB2Chain(raw, ctx); err != nil {
			c.logger().Debugf("read request ended: %v", err)
			c.close()
		}
	}()
	return true, nil
}

func verifyReadFrame(raw []byte, ctx pduCtx, sess *Session) error {
	if ctx.encrypted {
		return nil
	}
	for rest := raw; len(rest) != 0; {
		next := int(binary.LittleEndian.Uint32(rest[20:24]))
		segment := rest
		if next != 0 {
			segment = rest[:next]
		}
		signed := binary.LittleEndian.Uint32(segment[16:20])&smb.SMB2_FLAGS_SIGNED != 0
		if sess.Flags&smb.SessionFlagEncryptData != 0 || (!signed && sess.SigningRequired) ||
			(signed && sess.shouldVerify() && !sess.verifyPDU(segment)) {
			return fmt.Errorf("read frame protection verification failed")
		}
		if next == 0 {
			break
		}
		rest = rest[next:]
	}
	return nil
}

func (c *Conn) rejectReadFrame(ctx pduCtx, headers []smb.Header, status uint32) error {
	chain := &chainState{}
	ctx.chain = chain
	for _, h := range headers {
		if h.Flags&smb.SMB2_FLAGS_RELATED_OPERATIONS != 0 {
			h.SessionID, h.TreeID = headers[0].SessionID, headers[0].TreeID
		}
		ctx.signed = h.Flags&smb.SMB2_FLAGS_SIGNED != 0
		if err := c.writeRawError(ctx, &h, status); err != nil {
			return err
		}
	}
	return c.flushChain(ctx, chain)
}

func (c *Conn) beginReadRequest(ctx pduCtx, h *smb.Header) (pduCtx, func()) {
	job := ctx.readJob
	if job == nil {
		return ctx, func() {}
	}
	requestCtx, cancel := context.WithCancel(job.ctx)
	done := make(chan struct{})
	c.asyncMu.Lock()
	job.currentID, job.session, job.tree, job.handle = h.MessageID, h.SessionID, h.TreeID, 0
	job.active, job.requestCancel, job.requestDone = true, cancel, done
	canceled := job.canceled[h.MessageID]
	c.asyncMu.Unlock()
	if canceled {
		cancel()
	}
	ctx.readCtx = requestCtx
	return ctx, func() {
		cancel()
		c.asyncMu.Lock()
		job.active, job.requestCancel = false, nil
		close(done)
		c.asyncMu.Unlock()
	}
}

func (c *Conn) bindRead(ctx pduCtx, h *smb.Header, handle uint64) {
	if ctx.readJob == nil {
		return
	}
	c.asyncMu.Lock()
	ctx.readJob.session, ctx.readJob.tree, ctx.readJob.handle = h.SessionID, h.TreeID, handle
	c.asyncMu.Unlock()
}

func (c *Conn) cancelRead(id, session uint64) bool {
	c.asyncMu.Lock()
	job := c.readJobs[id]
	if job == nil || job.session != session {
		c.asyncMu.Unlock()
		return false
	}
	job.canceled[id] = true
	cancel := job.requestCancel
	active := job.active && job.currentID == id
	c.asyncMu.Unlock()
	if active && cancel != nil {
		cancel()
	}
	return true
}

// Zero selects every object at that level. Session/connection teardown cancels
// the frame; a file/tree close interrupts only the operation using that object.
func (c *Conn) cancelReads(session uint64, tree uint32, handle uint64, stop bool) {
	c.asyncMu.Lock()
	if stop {
		c.readStopping = true
	}
	jobs := make(map[*readJob]bool)
	for _, job := range c.readJobs {
		if (session == 0 || job.session == session) && (tree == 0 || job.tree == tree) &&
			(handle == 0 || job.handle == handle) {
			jobs[job] = true
		}
	}
	var cancels []context.CancelFunc
	for job := range jobs {
		if stop || (tree == 0 && handle == 0) {
			cancels = append(cancels, job.cancel)
		} else if job.active && job.requestCancel != nil {
			cancels = append(cancels, job.requestCancel)
		}
	}
	c.asyncMu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

func (c *Conn) waitReads(session uint64, tree uint32, handle uint64, except *readJob) {
	c.asyncMu.Lock()
	jobs := make(map[*readJob]bool)
	for _, job := range c.readJobs {
		if job != except && (session == 0 || job.session == session) &&
			(tree == 0 || job.tree == tree) && (handle == 0 || job.handle == handle) {
			jobs[job] = true
		}
	}
	var waits []<-chan struct{}
	for job := range jobs {
		if tree == 0 && handle == 0 {
			waits = append(waits, job.done)
		} else if job.active && job.requestDone != nil {
			waits = append(waits, job.requestDone)
		}
	}
	c.asyncMu.Unlock()
	for _, done := range waits {
		<-done
	}
}

func (ctx pduCtx) context() context.Context {
	if ctx.readCtx != nil {
		return ctx.readCtx
	}
	return context.Background()
}
