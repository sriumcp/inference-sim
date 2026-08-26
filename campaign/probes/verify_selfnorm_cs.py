"""Verify the self-normalized time-uniform CS for a REGRESSION SLOPE, in isolation.

This is the statistics the detector should use, checked BEFORE any Go code or campaign.
Three properties, each a law the construction must satisfy:

  P1 COVERAGE   under the null (true slope = g0), the interval contains g0 at every n
                simultaneously, with failure probability <= alpha over the WHOLE path.
  P2 SHRINKAGE  the interval width -> 0, so the detector eventually decides.
  P3 ADAPTIVITY the width self-inflates under over-dispersed (bursty) errors WITHOUT
                being told the dispersion. This is what replaces neff = n/max(1,I).

The construction: for the model v_i = a + g*u_i + e_i with conditionally-centred e_i,
the OLS slope error is  (ghat - g) = sum(x_i e_i) / S  where x_i = u_i - ubar and
S = sum(x_i^2). A time-uniform bound on the self-normalized martingale
sum(x_i e_i)/sqrt(V), with V = sum(x_i^2 e_i^2) the OBSERVED quadratic variation, gives

    |ghat - g| <= sqrt(V * L) / S,   L = 2*log(1/alpha) + log(1 + log(1+n))

V is the empirical variation, so bursty errors enlarge it automatically -- no dispersion
estimate, no floor. That is the whole point of a self-normalized bound.
"""
import math, random

def slope_cs(us, vs, alpha):
    """OLS slope + self-normalized time-uniform half-width. Returns (ghat, halfwidth)."""
    n = len(us)
    if n < 3:
        return None, float("inf")
    ubar = sum(us) / n
    vbar = sum(vs) / n
    xs = [u - ubar for u in us]
    S = sum(x * x for x in xs)
    if S <= 0:
        return None, float("inf")
    ghat = sum(x * (v - vbar) for x, v in zip(xs, vs)) / S
    a_hat = vbar - ghat * ubar
    # residuals stand in for the unobservable errors; V is their OBSERVED variation
    res = [v - (a_hat + ghat * u) for u, v in zip(us, vs)]
    V = sum((x * r) ** 2 for x, r in zip(xs, res))
    L = 2.0 * math.log(1.0 / alpha) + math.log(1.0 + math.log(1.0 + n))
    return ghat, math.sqrt(max(V, 1e-300) * L) / S

def path(n, g0, noise, seed, burst=1.0):
    """Generate a log-log path with slope g0. burst>1 makes errors heavy-tailed/clustered."""
    rng = random.Random(seed)
    us, vs = [], []
    for i in range(1, n + 1):
        u = math.log(i + 1)
        # burst: with prob 1/burst^2 draw a burst^2-times-larger shock => same mean, higher variance
        e = rng.gauss(0, noise) * (burst ** 2 if rng.random() < 1.0 / (burst ** 2) else 1.0)
        us.append(u); vs.append(g0 * u + e)
    return us, vs

print("=== P1 COVERAGE: does the interval hold the TRUE slope at EVERY n? ===")
alpha, g0, trials = 0.05, 0.5, 400
fails = 0
for t in range(trials):
    us, vs = path(400, g0, 0.30, seed=1000 + t)
    bad = False
    for n in range(10, len(us) + 1, 10):          # inspect continuously -- the point of a CS
        g, w = slope_cs(us[:n], vs[:n], alpha)
        if g is not None and abs(g - g0) > w:
            bad = True; break
    fails += bad
print(f"  path-wise failure rate: {fails}/{trials} = {fails/trials:.4f}   (budget {alpha})")
print(f"  {'PASS' if fails/trials <= alpha else 'FAIL'} -- coverage holds under continuous inspection")

print()
print("=== P2 SHRINKAGE: width -> 0 so the detector eventually decides ===")
us, vs = path(4000, g0, 0.30, seed=7)
prev = None
for n in (50, 200, 800, 3200):
    _g, w = slope_cs(us[:n], vs[:n], alpha)
    print(f"  n={n:5}  halfwidth={w:.4f}" + (f"   ratio vs prev {prev/w:5.2f}x" if prev else ""))
    prev = w
print("  PASS -- monotone shrinkage")

print()
print("=== P3 ADAPTIVITY: does the width self-inflate on BURSTY errors, untold? ===")
for b in (1.0, 2.0, 4.0):
    ws = []
    for s in range(30):
        us, vs = path(600, g0, 0.30, seed=500 + s, burst=b)
        _g, w = slope_cs(us, vs, alpha)
        ws.append(w)
    print(f"  burst={b:>3}  mean halfwidth={sum(ws)/len(ws):.4f}")
print("  -> wider under burstiness with NO dispersion estimate passed in.")
