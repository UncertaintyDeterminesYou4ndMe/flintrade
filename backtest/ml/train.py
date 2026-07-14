"""
Train a linear factor (+ optional gate) on 2025 data, validate on the 2025
chronological tail, then evaluate the final model on 2026 holdout.

Usage:
    backtest/ml/.venv/bin/python train.py
    .../python train.py --no-gate
    .../python train.py --tau 0.3 --lam-factor 1e-3 --epochs 800
"""

import argparse
import sys
from pathlib import Path

import numpy as np
import torch

HERE = Path(__file__).parent
sys.path.insert(0, str(HERE))

from features import build_dataset, FEATURES  # noqa: E402
from model import FactorModel, GateModel, compute_loss  # noqa: E402
from evaluate import evaluate, format_eval  # noqa: E402

DATA_2025 = HERE.parent / "data_1h_2025"
DATA_2026 = HERE.parent / "data_1h"


def to_tensors(ds, device):
    return {
        "X": torch.tensor(ds["X"], dtype=torch.float32, device=device),
        "G": (torch.tensor(ds["G"], dtype=torch.float32, device=device)
              if ds["G"] is not None else None),
        "fwd": torch.tensor(ds["fwd"], dtype=torch.float32, device=device),
        "valid_t": torch.tensor(ds["valid_t"], dtype=torch.bool, device=device),
    }


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--epochs", type=int, default=600)
    ap.add_argument("--lr", type=float, default=0.05)
    ap.add_argument("--tau", type=float, default=0.5)
    ap.add_argument("--lam-factor", type=float, default=5e-4)
    ap.add_argument("--lam-gate", type=float, default=1e-4)
    ap.add_argument("--min-trade-freq", type=float, default=0.20)
    ap.add_argument("--lam-freq", type=float, default=1e-2)
    ap.add_argument("--no-gate", action="store_true")
    ap.add_argument("--train-frac", type=float, default=0.70,
                    help="Fraction of 2025 used for training; rest is in-period validation.")
    ap.add_argument("--seed", type=int, default=0)
    ap.add_argument("--out", type=str, default=str(HERE / "weights.npz"))
    args = ap.parse_args()

    torch.manual_seed(args.seed)
    np.random.seed(args.seed)
    device = "cpu"

    # Load datasets.
    print("Loading data...")
    ds_2025 = build_dataset(DATA_2025, gate=not args.no_gate)
    ds_2026 = build_dataset(DATA_2026, gate=not args.no_gate)
    print(f"  2025: {ds_2025['X'].shape}, valid_t={ds_2025['valid_t'].sum()}")
    print(f"  2026: {ds_2026['X'].shape}, valid_t={ds_2026['valid_t'].sum()}")
    print(f"  factor features: {ds_2025['feature_names']}")
    print(f"  gate features:   {ds_2025['gate_names']}")

    # 2025 train / val split — chronological.
    T_2025 = ds_2025["X"].shape[0]
    split = int(T_2025 * args.train_frac)
    train_mask = ds_2025["valid_t"].copy()
    train_mask[split:] = False
    val_mask = ds_2025["valid_t"].copy()
    val_mask[:split] = False

    # Standardize features using TRAIN portion only (no leakage).
    # X: (T, N, F) — flatten over (t, n) within train_mask, compute mean/std per F.
    train_X = ds_2025["X"][train_mask]                    # (T_trn*N, F)
    feat_mu = train_X.reshape(-1, train_X.shape[-1]).mean(axis=0)
    feat_std = train_X.reshape(-1, train_X.shape[-1]).std(axis=0)
    feat_std = np.where(feat_std > 1e-8, feat_std, 1.0)
    ds_2025["X"] = (ds_2025["X"] - feat_mu) / feat_std
    ds_2026["X"] = (ds_2026["X"] - feat_mu) / feat_std

    if ds_2025["G"] is not None:
        train_G = ds_2025["G"][train_mask]
        gate_mu = train_G.mean(axis=0)
        gate_std = np.where(train_G.std(axis=0) > 1e-8, train_G.std(axis=0), 1.0)
        ds_2025["G"] = (ds_2025["G"] - gate_mu) / gate_std
        ds_2026["G"] = (ds_2026["G"] - gate_mu) / gate_std
    else:
        gate_mu = gate_std = None

    print(f"  feature mu: {dict(zip(ds_2025['feature_names'], feat_mu.round(4)))}")
    print(f"  feature std: {dict(zip(ds_2025['feature_names'], feat_std.round(4)))}")

    # Build models.
    n_feat = ds_2025["X"].shape[2]
    factor = FactorModel(n_feat).to(device)
    gate = None
    if not args.no_gate and ds_2025["G"] is not None:
        n_gate = ds_2025["G"].shape[1]
        gate = GateModel(n_gate).to(device)

    params = list(factor.parameters())
    if gate is not None:
        params += list(gate.parameters())
    opt = torch.optim.Adam(params, lr=args.lr)

    # Move to torch.
    t25 = to_tensors(ds_2025, device)
    train_t = torch.tensor(train_mask, dtype=torch.bool, device=device)
    val_t = torch.tensor(val_mask, dtype=torch.bool, device=device)

    # Train.
    best_val = float("inf")
    best_state = None
    for epoch in range(args.epochs):
        factor.train()
        if gate: gate.train()
        opt.zero_grad()
        loss, diag = compute_loss(
            factor, gate, t25["X"], t25["G"], t25["fwd"], train_t,
            tau=args.tau,
            lam_factor_l1=args.lam_factor,
            lam_gate_l1=args.lam_gate,
            min_trade_freq=args.min_trade_freq,
            lam_freq=args.lam_freq,
        )
        loss.backward()
        opt.step()

        if epoch % 50 == 0 or epoch == args.epochs - 1:
            with torch.no_grad():
                _, val_diag = compute_loss(
                    factor, gate, t25["X"], t25["G"], t25["fwd"], val_t,
                    tau=args.tau,
                    lam_factor_l1=args.lam_factor, lam_gate_l1=args.lam_gate,
                    min_trade_freq=args.min_trade_freq, lam_freq=args.lam_freq,
                )
                print(
                    f"  epoch {epoch:4d}  "
                    f"trn_pr={diag['mean_portfolio_ret']*1e4:+6.2f}bps  "
                    f"val_pr={val_diag['mean_portfolio_ret']*1e4:+6.2f}bps  "
                    f"trn_loss={diag['main_loss']:+.5f}  "
                    f"gate_trn={diag['mean_gate']:.2f}  gate_val={val_diag['mean_gate']:.2f}"
                )
                if val_diag["main_loss"] < best_val:
                    best_val = val_diag["main_loss"]
                    best_state = {
                        "factor_w": factor.weights.detach().cpu().numpy().copy(),
                        "gate_w": (gate.weights.detach().cpu().numpy().copy() if gate else None),
                        "gate_b": (float(gate.bias.detach()) if gate else None),
                        "epoch": epoch,
                    }

    if best_state is None:
        best_state = {
            "factor_w": factor.weights.detach().cpu().numpy(),
            "gate_w": (gate.weights.detach().cpu().numpy() if gate else None),
            "gate_b": (float(gate.bias.detach()) if gate else None),
            "epoch": args.epochs - 1,
        }

    # Evaluate using HARD top-1 selection.
    print()
    print("=" * 110)
    print(f"Final eval — using best-val checkpoint (epoch {best_state['epoch']})")
    print("=" * 110)
    print("Factor feature weights:")
    for n, w in zip(ds_2025["feature_names"], best_state["factor_w"]):
        print(f"  {n:14s}  {w:+.4f}")
    if gate:
        print("Gate feature weights:")
        for n, w in zip(ds_2025["gate_names"], best_state["gate_w"]):
            print(f"  {n:18s}  {w:+.4f}")
        print(f"  bias              {best_state['gate_b']:+.4f}")

    print()
    print("(IC = rank IC of all valid t, ignores gate; trades/cov/hit/sharpe = top-1 with gate)")
    eval_args = (
        best_state["factor_w"], best_state["gate_w"], best_state["gate_b"],
    )
    s_train = evaluate(*eval_args,
                       X=ds_2025["X"], G=ds_2025["G"], fwd=ds_2025["fwd"],
                       valid_t=train_mask)
    s_val = evaluate(*eval_args,
                     X=ds_2025["X"], G=ds_2025["G"], fwd=ds_2025["fwd"],
                     valid_t=val_mask)
    s_holdout = evaluate(*eval_args,
                         X=ds_2026["X"], G=ds_2026["G"], fwd=ds_2026["fwd"],
                         valid_t=ds_2026["valid_t"])
    print(format_eval("2025-train", s_train))
    print(format_eval("2025-val",   s_val))
    print(format_eval("2026-OUT",   s_holdout))

    # Save weights + scaling so inference can replicate exactly.
    save_kwargs = dict(
        factor_w=best_state["factor_w"],
        gate_w=(best_state["gate_w"] if best_state["gate_w"] is not None else np.array([])),
        gate_b=np.array([best_state["gate_b"]] if best_state["gate_b"] is not None else []),
        feature_names=np.array(ds_2025["feature_names"]),
        gate_names=np.array(ds_2025["gate_names"]),
        feat_mu=feat_mu, feat_std=feat_std,
        tau=np.array([args.tau]),
    )
    if gate_mu is not None:
        save_kwargs["gate_mu"] = gate_mu
        save_kwargs["gate_std"] = gate_std
    np.savez(args.out, **save_kwargs)
    print(f"\nSaved to {args.out}")


if __name__ == "__main__":
    main()
