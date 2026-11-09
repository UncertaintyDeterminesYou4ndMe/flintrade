// Package settle is the one accounting path shared by the executor (immediate
// fills) and the reconciler (deferred limit fills), ported from agent/settle.py.
// Both must book through here or the ledger drifts.
//
// Equity is cash-like, positions marked at entry:
//
//	open:  equity -= commission; open_risk += risk_amt
//	close: equity += gross - commission; open_risk -= released risk;
//	       day_realized_pnl += net
package settle

import (
	"context"
	"errors"
	"fmt"
	"math"

	"flintrade/internal/db"
	"flintrade/internal/pyfloat"
)

func r4(x float64) float64 { return pyfloat.Round(x, 4) }

// Attribution maps an intent source to the strategy/manual taxonomy. The live
// path never produces test/outage-degraded; only backfills write those.
func Attribution(source string) string {
	if source == "user" {
		return "manual"
	}
	return "strategy"
}

// OpenInput describes an entry fill. Side is long | short.
type OpenInput struct {
	Symbol             string
	Side               string
	Qty                int
	FillPrice          float64
	Stop               *float64
	Target, Target2    *float64
	CommissionPerShare float64
	Source             string
	IntentID           *int64
	BrokerOrderID      *string
	Reason             *string
	Features           any
}

// Open books an entry: position + trade + risk state in one transaction.
func Open(ctx context.Context, s *db.Store, in OpenInput) (int64, error) {
	comm := r4(float64(float64(in.Qty) * in.CommissionPerShare))
	var riskAmt *float64
	if in.Stop != nil && *in.Stop != 0 {
		v := r4(float64(float64(in.Qty) * math.Abs(in.FillPrice-*in.Stop)))
		riskAmt = &v
	}
	action := "BUY"
	if in.Side == "short" {
		action = "SHORT"
	}
	src := nullable(in.Source)
	var posID int64
	err := s.Tx(ctx, func(tx *db.Store) error {
		var err error
		posID, err = tx.OpenPosition(ctx, db.PositionInput{Symbol: in.Symbol, Side: in.Side, Qty: in.Qty,
			EntryPrice: in.FillPrice, Stop: in.Stop, Target: in.Target, Target2: in.Target2,
			RiskAmt: riskAmt, Source: src, IntentID: in.IntentID})
		if err != nil {
			return err
		}
		if _, err := tx.AppendTrade(ctx, db.TradeInput{Symbol: in.Symbol, Action: action, Qty: in.Qty,
			FillPrice: in.FillPrice, Commission: comm, Source: src, IntentID: in.IntentID,
			BrokerOrderID: in.BrokerOrderID, PositionID: &posID, Features: in.Features, Reason: in.Reason,
			Attribution: db.Ptr(Attribution(in.Source))}); err != nil {
			return err
		}
		r, err := risk(ctx, tx)
		if err != nil {
			return err
		}
		add := 0.0
		if riskAmt != nil {
			add = *riskAmt
		}
		return tx.UpdateRisk(ctx, db.Fields{"equity": r4(*r.Equity - comm), "open_risk": r4(r.OpenRisk + add)})
	})
	return posID, err
}

// CloseInput describes an exit fill against a held position. Qty below the
// held qty is a partial close (first take-profit): the row stays open, shrinks,
// releases risk pro rata and gets t1_done.
type CloseInput struct {
	Held               db.Position
	FillPrice          float64
	Qty                int
	CommissionPerShare float64
	IntentID           *int64
	BrokerOrderID      *string
	Reason             *string
	// FullExitFilling: this is one fill of an order meant to close the whole
	// position. The row shrinks but is not marked as a first take-profit.
	FullExitFilling bool
}

// Close books an exit and returns the net PnL.
func Close(ctx context.Context, s *db.Store, in CloseInput) (float64, error) {
	h := in.Held
	partial := in.Qty > 0 && in.Qty < h.Qty
	comm := r4(float64(float64(in.Qty) * in.CommissionPerShare))
	// float64() on every product: Go may fuse a*b+c into an FMA, even across
	// statements, which would make the ledger differ from Python in the last bit.
	var gross float64
	if h.Side == "long" {
		gross = float64((in.FillPrice - h.EntryPrice) * float64(in.Qty))
	} else {
		gross = float64((h.EntryPrice - in.FillPrice) * float64(in.Qty))
	}
	pnl := r4(gross - comm)
	action := "SELL"
	if h.Side != "long" {
		action = "COVER"
	}
	heldRisk := 0.0
	if h.RiskAmt != nil {
		heldRisk = *h.RiskAmt
	}
	// Partial: release pro rata. Full: release everything (no rounding tail).
	released := heldRisk
	if partial {
		released = r4(float64(heldRisk*float64(in.Qty)) / float64(h.Qty))
	}
	source := ""
	if h.Source != nil {
		source = *h.Source
	}
	err := s.Tx(ctx, func(tx *db.Store) error {
		if partial {
			var remaining *float64
			if h.RiskAmt != nil {
				remaining = db.Ptr(r4(heldRisk - released))
			}
			shrink := tx.ReducePosition
			if in.FullExitFilling {
				shrink = tx.ShrinkPosition
			}
			if err := shrink(ctx, h.ID, h.Qty-in.Qty, remaining); err != nil {
				return err
			}
		} else if err := tx.ClosePosition(ctx, h.ID); err != nil {
			return err
		}
		if _, err := tx.AppendTrade(ctx, db.TradeInput{Symbol: h.Symbol, Action: action, Qty: in.Qty,
			FillPrice: in.FillPrice, Commission: comm, PnL: &pnl, Source: h.Source, IntentID: in.IntentID,
			BrokerOrderID: in.BrokerOrderID, PositionID: &h.ID, Reason: in.Reason,
			Attribution: db.Ptr(Attribution(source))}); err != nil {
			return err
		}
		r, err := risk(ctx, tx)
		if err != nil {
			return err
		}
		return tx.UpdateRisk(ctx, db.Fields{
			"equity":           r4(*r.Equity + gross - comm),
			"open_risk":        r4(math.Max(0, r.OpenRisk-released)),
			"day_realized_pnl": r4(r.DayRealizedPnL + pnl),
		})
	})
	return pnl, err
}

// AddInput is a later fill of the entry order that opened Held.
type AddInput struct {
	Held               db.Position
	Qty                int
	FillPrice          float64
	CommissionPerShare float64
	IntentID           *int64
	BrokerOrderID      *string
	Reason             *string
}

// Add books a further fill of an entry order into the position its first
// fill opened: qty grows, entry becomes the share-weighted average, risk is
// recomputed against the stop and the difference added to open_risk.
func Add(ctx context.Context, s *db.Store, in AddInput) error {
	h := in.Held
	if h.Status != "open" {
		return fmt.Errorf("position %d is %s, cannot add a fill to it", h.ID, h.Status)
	}
	comm := r4(float64(float64(in.Qty) * in.CommissionPerShare))
	total := h.Qty + in.Qty
	entry := r4((float64(float64(h.Qty)*h.EntryPrice) + float64(float64(in.Qty)*in.FillPrice)) / float64(total))
	var riskAmt *float64
	oldRisk, newRisk := 0.0, 0.0
	if h.RiskAmt != nil {
		oldRisk = *h.RiskAmt
	}
	if h.Stop != nil && *h.Stop != 0 {
		newRisk = r4(float64(float64(total) * math.Abs(entry-*h.Stop)))
		riskAmt = &newRisk
	}
	action := "BUY"
	if h.Side == "short" {
		action = "SHORT"
	}
	source := ""
	if h.Source != nil {
		source = *h.Source
	}
	return s.Tx(ctx, func(tx *db.Store) error {
		if err := tx.AddToPosition(ctx, h.ID, total, entry, riskAmt); err != nil {
			return err
		}
		if _, err := tx.AppendTrade(ctx, db.TradeInput{Symbol: h.Symbol, Action: action, Qty: in.Qty,
			FillPrice: in.FillPrice, Commission: comm, Source: h.Source, IntentID: in.IntentID,
			BrokerOrderID: in.BrokerOrderID, PositionID: &h.ID, Reason: in.Reason,
			Attribution: db.Ptr(Attribution(source))}); err != nil {
			return err
		}
		r, err := risk(ctx, tx)
		if err != nil {
			return err
		}
		return tx.UpdateRisk(ctx, db.Fields{"equity": r4(*r.Equity - comm), "open_risk": r4(math.Max(0, r.OpenRisk+newRisk-oldRisk))})
	})
}

func risk(ctx context.Context, s *db.Store) (*db.RiskState, error) {
	r, err := s.GetRisk(ctx)
	if err != nil {
		return nil, err
	}
	if r == nil || r.Equity == nil {
		return nil, errors.New("risk_state has no equity: cannot settle")
	}
	return r, nil
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
