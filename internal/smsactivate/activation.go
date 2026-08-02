package smsactivate

import (
	"context"
	"sync"
	"time"
)

type Activation struct {
	client *Client

	ActivationID string
	Number       string
	E164         string
	Country      int

	mu           sync.Mutex
	pollCount    int
	codeReceived bool
	closed       bool
	closing      bool
	closeDone    chan struct{}
	closeStatus  int
	closeResult  string
	closeErr     error
}

func newActivation(client *Client, number Number) *Activation {
	e164 := number.Phone
	if len(e164) > 0 && e164[0] != '+' {
		e164 = "+" + e164
	}
	return &Activation{
		client:       client,
		ActivationID: number.ActivationID,
		Number:       number.Phone,
		E164:         e164,
		Country:      number.Country,
	}
}

func (a *Activation) PollCode(ctx context.Context, interval time.Duration) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if interval <= 0 {
		return "", configError("轮询间隔须大于 0")
	}
	a.mu.Lock()
	if a.closed || a.closing {
		a.mu.Unlock()
		return "", &Error{Code: "ACTIVATION_CLOSED", Message: "激活已结束"}
	}
	status := 1
	if a.pollCount > 0 {
		status = 3
	}
	a.pollCount++
	a.mu.Unlock()
	if _, err := a.client.SetStatus(ctx, a.ActivationID, status); err != nil {
		return "", err
	}
	for {
		current, err := a.client.GetStatus(ctx, a.ActivationID)
		if err != nil {
			return "", err
		}
		switch current.State {
		case StatusOK, StatusRetry:
			if current.Code != "" {
				a.mu.Lock()
				a.codeReceived = true
				a.mu.Unlock()
				return current.Code, nil
			}
		case StatusCancel:
			return "", &Error{Code: "STATUS_CANCEL", Message: "激活已被取消"}
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return "", ctx.Err()
		case <-timer.C:
		}
	}
}

func (a *Activation) Close(ctx context.Context, success bool) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		a.mu.Lock()
		if a.closed {
			result, err := a.closeResult, a.closeErr
			a.mu.Unlock()
			return result, err
		}
		if a.closing {
			done := a.closeDone
			a.mu.Unlock()
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		a.closing = true
		a.closeDone = make(chan struct{})
		done := a.closeDone
		status := a.closeStatus
		if status == 0 {
			status = 8
			if success && a.codeReceived {
				status = 6
			}
			a.closeStatus = status
		}
		a.mu.Unlock()

		result, err := a.client.SetStatus(ctx, a.ActivationID, status)
		a.mu.Lock()
		a.closeResult = result
		a.closeErr = err
		a.closing = false
		a.closed = err == nil
		close(done)
		a.mu.Unlock()
		return result, err
	}
}
