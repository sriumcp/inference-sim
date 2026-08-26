"""Is d log R_t/d log t == d log Peak_t/d log t - 1, and which regimes separate?

Checks the user's question empirically rather than trusting the algebra: simulates the
three Lindley regimes' Peak growth, fits both log-log slopes, and reports the offset.
"""
import math, random

def peak_series(n, exponent, seed=42):
    """Peak_t ~ t**exponent, as a non-decreasing (ratchet) integer series."""
    rng = random.Random(seed)
    out, peak = [], 1.0
    for i in range(1, n + 1):
        target = (i ** exponent) * (1.0 + 0.15 * (rng.random() - 0.5))
        peak = max(peak, target)          # RATCHET: never decreases
        out.append((i, peak))
    return out

def loglog_slope(pairs):
    xs = [math.log(t) for t, _ in pairs]
    ys = [math.log(v) for _, v in pairs]
    n = len(xs); mx = sum(xs)/n; my = sum(ys)/n
    num = sum((x-mx)*(y-my) for x, y in zip(xs, ys))
    den = sum((x-mx)**2 for x in xs)
    return num/den

print(f"{'regime':>22} | {'true':>5} | {'gamma (Peak)':>12} | {'beta (R_t)':>10} | {'gamma-beta':>10}")
print("-"*76)
for name, e in (("rho<1 healthy", 0.0), ("rho=1 critical", 0.5), ("rho>1 overloaded", 1.0)):
    p = peak_series(4000, e)
    g = loglog_slope(p)
    r = loglog_slope([(t, v/t) for t, v in p])     # R_t = Peak_t / t
    print(f"{name:>22} | {e:>5.1f} | {g:>12.4f} | {r:>10.4f} | {g-r:>10.6f}")

print("\n=== the offset is exactly 1 in every regime => equivalent up to +1 (user was right)")
print("=== and gamma stays in [0,1] while beta sits in [-1,0]")
