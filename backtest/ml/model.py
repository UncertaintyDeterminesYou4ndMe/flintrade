"""
Linear factor + sigmoid gate models. Differentiable end-to-end.

Forward:
    scores = X @ w_factor                       # (T, N)
    weights = softmax(scores / tau, dim=N)      # (T, N) — soft portfolio
    gate = sigmoid(b + G @ w_gate)              # (T,)
    expected_per_t = (weights * fwd).sum(N)     # (T,)
    portfolio_ret = gate * expected_per_t       # (T,)

Loss:
    L = -mean(portfolio_ret) + λ_factor * |w_factor|_1 + λ_gate * |w_gate|_1
        + (optional) low-trade penalty if mean(gate) < min_trade_freq
"""

import torch
import torch.nn as nn
import torch.nn.functional as F


class FactorModel(nn.Module):
    def __init__(self, n_features: int):
        super().__init__()
        self.weights = nn.Parameter(torch.zeros(n_features))

    def forward(self, X):
        # X: (T, N, F) -> (T, N)
        return torch.einsum("tnf,f->tn", X, self.weights)


class GateModel(nn.Module):
    def __init__(self, n_gate_features: int, init_bias: float = 2.0):
        super().__init__()
        self.weights = nn.Parameter(torch.zeros(n_gate_features))
        # init_bias=2.0 -> sigmoid(2)=~0.88 so gate starts near always-on,
        # lets the factor signal speak first; gate then learns when to gate down.
        self.bias = nn.Parameter(torch.tensor(float(init_bias)))

    def forward(self, G):
        # G: (T, F_gate) -> (T,)
        return torch.sigmoid(self.bias + G @ self.weights)


def compute_loss(
    factor: FactorModel,
    gate: GateModel | None,
    X: torch.Tensor,        # (T, N, F)
    G: torch.Tensor | None, # (T, F_gate) or None
    fwd: torch.Tensor,      # (T, N)
    valid_t: torch.Tensor,  # (T,) bool
    *,
    tau: float = 0.5,       # softmax temperature; lower = sharper (closer to argmax)
    lam_factor_l1: float = 1e-3,
    lam_gate_l1: float = 1e-4,
    min_trade_freq: float = 0.2,  # softly penalize if mean(gate) drops below this
    lam_freq: float = 1e-2,
):
    scores = factor(X)                            # (T, N)
    w = F.softmax(scores / tau, dim=1)            # (T, N)
    expected = (w * fwd).sum(dim=1)               # (T,)

    if gate is not None and G is not None:
        g = gate(G)                               # (T,)
    else:
        g = torch.ones(X.shape[0], device=X.device)

    portfolio_ret = g * expected                  # (T,)

    # Mask invalid timestamps (warmup, last bar).
    pr = portfolio_ret[valid_t]
    g_v = g[valid_t]

    main_loss = -pr.mean()

    reg = lam_factor_l1 * factor.weights.abs().sum()
    if gate is not None:
        reg = reg + lam_gate_l1 * gate.weights.abs().sum()

    # Penalize gate that's mostly closed (we want a useful signal, not "skip everything").
    mean_g = g_v.mean()
    freq_penalty = lam_freq * F.relu(min_trade_freq - mean_g)

    loss = main_loss + reg + freq_penalty

    with torch.no_grad():
        diagnostics = {
            "loss": loss.item(),
            "main_loss": main_loss.item(),
            "mean_portfolio_ret": pr.mean().item(),
            "mean_gate": mean_g.item(),
            "reg": reg.item(),
            "freq_penalty": freq_penalty.item(),
        }
    return loss, diagnostics
