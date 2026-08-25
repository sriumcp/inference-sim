import sys; sys.path.insert(0, "../bench")
from score_detector import detection_delay

def mk(n, first_fire_idx, clears_at=None):
    recs = []
    for i in range(n):
        fired = i >= first_fire_idx and (clears_at is None or i < clears_at)
        recs.append({"timestamp": i*1000,
                     "result":{"level":"OVERLOADED" if fired else "STABLE"}})
    return recs

n = 2000
print("=== the case that used to collapse: composite(3) vs randomwalk(21) ===")
for name, idx in (("composite", 3), ("randomwalk", 21)):
    d, i, c = detection_delay(mk(n, idx))
    print(f"  {name:>11}: delay={d:>8} us  index={i:>4}  clipped={c}")

print("\n=== full sweep (old metric reported 0 for every idx < 200) ===")
print(f"  {'idx':>5} | {'delay_us':>9} | {'index':>5} | clipped")
for idx in (3, 21, 100, 199, 201, 400, 1000):
    d, i, c = detection_delay(mk(n, idx))
    print(f"  {idx:>5} | {str(d):>9} | {str(i):>5} | {c}")

print("\n=== transient rejection: fires at 5, clears at 50, warm-up ends at 200 ===")
d, i, c = detection_delay(mk(n, 5, clears_at=50))
print(f"  delay={d} index={i} clipped={c}   (None => blip correctly NOT scored)")

print("\n=== never fires ===")
d, i, c = detection_delay(mk(n, n+1))
print(f"  delay={d} index={i} clipped={c}")

print("\n=== MONOTONICITY: strictly increasing in first-fire index? ===")
vals = [detection_delay(mk(n, k))[0] for k in (3,21,100,199,201,400,1000)]
print(f"  {vals}")
print(f"  strictly increasing: {all(a<b for a,b in zip(vals, vals[1:]))}")
