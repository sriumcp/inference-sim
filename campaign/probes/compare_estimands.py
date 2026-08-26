"""Which estimand discriminates saturation on a CAPPED queue? Measured on real BLIS traces.

The cumulative log-log exponent fails: Peak_t ~ t^gamma with gamma=1 assumes an UNBOUNDED
queue, but a real server pins at its concurrency/KV ceiling, so an overloaded run's Peak goes
flat and the cumulative slope decays toward 0 -- the same place a healthy run ends up.
Measured at 2x the cliff: gamma 0.80 -> 0.44 with peak pinned at 441.

Three candidates, scored by how well they separate sub- from super-capacity.
"""
import json, math

RATES = [26, 53, 79, 88, 106, 176]   # measured cliff = 88 rps
CLIFF = 88.0

def load(rate):
    tr = json.load(open(f"/tmp/tr{rate}.json"))["trace"]
    return [(s.get("observations", 0), s.get("peak_backlog", 0), s.get("in_flight", 0))
            for s in (rec["result"]["signals"] for rec in tr)]

def slope(points):
    if len(points) < 3:
        return float("nan")
    us = [math.log(n) for n, _ in points]
    vs = [math.log(p) for _, p in points]
    m = len(us); ub = sum(us)/m; vb = sum(vs)/m
    sxx = sum((u-ub)**2 for u in us)
    if sxx <= 0:
        return float("nan")
    return sum((u-ub)*(v-vb) for u, v in zip(us, vs))/sxx

rows = []
for rate in RATES:
    d = load(rate)
    peaks = [(n, p) for n, p, _ in d if n > 0 and p > 0]
    if len(peaks) < 50:
        continue
    cum = slope(peaks)
    trail = slope(peaks[int(len(peaks)*0.7):])          # last 30% only
    maxp = max(p for _, p, _ in d)
    tail = d[int(len(d)*0.7):]
    occ = sum(1 for _, _, f in tail if maxp > 0 and f >= 0.95*maxp)/max(1, len(tail))
    rows.append((rate, cum, trail, occ))

print(f"{'rate':>5} {'x cliff':>8} {'cum gamma':>10} {'trail gamma':>12} {'ceil occup':>11}")
for rate, cum, trail, occ in rows:
    print(f"{rate:>5} {rate/CLIFF:>8.2f} {cum:>10.3f} {trail:>12.3f} {occ:>11.3f}")

print()
sub = [r for r in rows if r[0] < CLIFF]
sup = [r for r in rows if r[0] > CLIFF]
print("=== SEPARATION: worst super-capacity value vs best sub-capacity value ===")
for idx, name in ((1, "cumulative gamma"), (2, "trailing gamma"), (3, "ceiling occupancy")):
    if not sub or not sup:
        continue
    worst_sup = min(r[idx] for r in sup)
    best_sub = max(r[idx] for r in sub)
    gap = worst_sup - best_sub
    verdict = "SEPARATES" if gap > 0 else "OVERLAPS"
    print(f"  {name:20} super_min={worst_sup:+.3f}  sub_max={best_sub:+.3f}  gap={gap:+.3f}  {verdict}")
