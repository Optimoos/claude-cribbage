package server

import (
	"context"
	"crypto/rand"
	"errors"
	"strings"
	"sync"
	"time"

	"cribbage/internal/auth"
	"cribbage/internal/protocol/pb"
	"cribbage/internal/store"
)

// Signing in by approval: a device that cannot sign in by itself (the
// desktop app, which cannot use passkeys or open emailed links) shows a
// short code, and the player approves it from a device that is signed in,
// usually the web client. The device holds a secret challenge too, so only
// it can claim the approval; the code alone is worth nothing to anyone
// who sees it, except to give that device the approver's account. That is
// why approving warns to approve only a code shown on one's own device.

const (
	deviceLifetime = 10 * time.Minute
	maxDevices     = 10000
	// maxDevicesPerAddr bounds one address's share of the waiting
	// sign-ins, so a few cannot fill the table for everyone.
	maxDevicesPerAddr = 5
	// deviceAlphabet has no vowels (no words) or easily confused letters.
	deviceAlphabet = "BCDFGHJKLMNPQRSTVWXZ"
)

type deviceReq struct {
	code    string
	expires time.Time
	// waiter is the connection told when the code is approved.
	waiter   *Conn
	approved *store.User
	// session is the approver's session token: the approval stands only
	// while that session does, so signing it out withdraws the approval.
	session string
	addr    string // limiter key of the address that asked ("" if unlimited)
}

type devices struct {
	mu     sync.Mutex
	byID   map[string]*deviceReq // by challenge
	byCode map[string]string     // code -> challenge
	byAddr map[string]int        // requests waiting, by addr
}

func newDevices() devices {
	return devices{byID: map[string]*deviceReq{}, byCode: map[string]string{}, byAddr: map[string]int{}}
}

// remove forgets a request. Callers hold mu.
func (d *devices) remove(id string) {
	r := d.byID[id]
	if r == nil {
		return
	}
	delete(d.byID, id)
	if d.byCode[r.code] == id {
		delete(d.byCode, r.code)
	}
	if r.addr != "" {
		if d.byAddr[r.addr]--; d.byAddr[r.addr] <= 0 {
			delete(d.byAddr, r.addr)
		}
	}
}

// sweep forgets expired requests. Callers hold mu.
func (d *devices) sweep(now time.Time) {
	for id, r := range d.byID {
		if now.After(r.expires) {
			d.remove(id)
		}
	}
}

// dropApprovals withdraws every approval userID has given.
func (d *devices) dropApprovals(userID int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for id, r := range d.byID {
		if r.approved != nil && r.approved.ID == userID {
			d.remove(id)
		}
	}
}

func newDeviceCode() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	for i := range b {
		b[i] = deviceAlphabet[int(b[i])%len(deviceAlphabet)] // 256 % 20 bias is harmless here
	}
	return string(b)
}

// normalDeviceCode keeps the letters of a typed code, upper-cased.
func normalDeviceCode(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		if r >= 'A' && r <= 'Z' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func showDeviceCode(c string) string { return c[:4] + "-" + c[4:] }

func (s *Server) startDeviceSignIn(c *Conn, id uint32) {
	if c.user != nil {
		c.sendError(id, pb.ErrorCode_ERROR_CODE_ALREADY_AUTHENTICATED, "", "already signed in")
		return
	}
	if !s.allowFrom(c, id, s.attempts) {
		return
	}
	now := time.Now()
	d := &s.devices
	d.mu.Lock()
	full := func() bool {
		return len(d.byID) >= maxDevices || (c.addr != "" && d.byAddr[c.addr] >= maxDevicesPerAddr)
	}
	if full() {
		d.sweep(now)
	}
	if full() {
		d.mu.Unlock()
		c.sendError(id, pb.ErrorCode_ERROR_CODE_RATE_LIMITED, "", "too many sign-ins waiting; try again later")
		return
	}
	challenge := auth.RandomID(32)
	code := newDeviceCode()
	for d.byCode[code] != "" {
		code = newDeviceCode()
	}
	d.byID[challenge] = &deviceReq{code: code, expires: now.Add(deviceLifetime), addr: c.addr}
	d.byCode[code] = challenge
	if c.addr != "" {
		d.byAddr[c.addr]++
	}
	d.mu.Unlock()
	c.signingIn.Store(true)
	url := ""
	if base := s.auth.BaseURL; base != "" {
		url = strings.TrimSuffix(base, "/") + "/#device=" + showDeviceCode(code)
	}
	c.send(&pb.ServerMessage{RequestId: id, Body: &pb.ServerMessage_DeviceCode{DeviceCode: &pb.DeviceCode{
		Challenge: challenge, Code: showDeviceCode(code), Url: url, ExpiresIn: uint32(deviceLifetime.Seconds()),
	}}})
}

// claimDeviceSignIn signs c in if its request was approved, or makes c the
// connection told when it is.
func (s *Server) claimDeviceSignIn(c *Conn, id uint32, challenge string) {
	if !s.allowSignIn(c, id) {
		return
	}
	if c.user != nil {
		c.sendError(id, pb.ErrorCode_ERROR_CODE_ALREADY_AUTHENTICATED, "", "already signed in")
		return
	}
	d := &s.devices
	d.mu.Lock()
	r := d.byID[challenge]
	if r == nil || time.Now().After(r.expires) {
		d.mu.Unlock()
		c.sendError(id, pb.ErrorCode_ERROR_CODE_NOT_FOUND, "", "this code has expired; start again")
		return
	}
	u, session := r.approved, r.session
	if u == nil {
		r.waiter = c
		d.mu.Unlock()
		c.signingIn.Store(true)
		c.send(&pb.ServerMessage{RequestId: id, Body: &pb.ServerMessage_Ack{Ack: &pb.Ack{}}})
		return
	}
	d.remove(challenge)
	d.mu.Unlock()
	s.authenticate(c, id, func(ctx context.Context) (*auth.Result, error) {
		fresh, err := s.auth.Store.UserByID(ctx, u.ID)
		if errors.Is(err, store.ErrNotFound) {
			return nil, auth.ErrBadToken
		}
		if err != nil {
			return nil, err
		}
		res, err := s.auth.SignIn(ctx, fresh)
		if err != nil {
			return nil, err
		}
		// Checked after the new session exists, so a sign-out that ends
		// the approver's session either sees the new one or is seen here.
		if uid, _, err := s.auth.Store.SessionUser(ctx, session); err != nil || uid != u.ID {
			_ = s.auth.Logout(ctx, res.Token)
			if err != nil && !errors.Is(err, store.ErrNotFound) {
				return nil, err
			}
			return nil, auth.ErrBadToken
		}
		return res, nil
	})
}

// approveDevice approves the device showing code as c's user.
func (s *Server) approveDevice(c *Conn, id uint32, code string) {
	if limited(c) && !s.codeTries.Take(c.userKey, time.Now()) {
		c.sendError(id, pb.ErrorCode_ERROR_CODE_RATE_LIMITED, "", "too many attempts; try again later")
		return
	}
	code = normalDeviceCode(code)
	d := &s.devices
	d.mu.Lock()
	challenge := d.byCode[code]
	r := d.byID[challenge]
	if r == nil || time.Now().After(r.expires) {
		d.mu.Unlock()
		c.sendError(id, pb.ErrorCode_ERROR_CODE_NOT_FOUND, "", "no device is showing that code; check it, or start again on the device")
		return
	}
	delete(d.byCode, code) // one approval per code
	r.approved, r.session = c.user, c.token
	waiter := r.waiter
	d.mu.Unlock()
	s.log.Info("device approved", "user", c.user.ID)
	c.send(&pb.ServerMessage{RequestId: id, Body: &pb.ServerMessage_Ack{Ack: &pb.Ack{}}})
	if waiter != nil {
		waiter.send(&pb.ServerMessage{Body: &pb.ServerMessage_DeviceApproved{DeviceApproved: &pb.DeviceApproved{}}})
	}
}
