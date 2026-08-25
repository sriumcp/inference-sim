# Does detection_delay_us clip to 0 whenever the first fire lands in the warm-up?
def detection_delay_us(records, warmup_frac=0.1):
    if not records: return None
    start = int(len(records) * warmup_frac)
    tail = records[start:]
    if not tail: return None
    t0 = tail[0]["timestamp"]
    for r in tail:
        if r["result"]["level"] in ("BACKLOGGED","OVERLOADED"):
            return max(0, r["timestamp"] - t0)
    return None

def mk(n, first_fire_idx):
    return [{"timestamp": i*1000,
             "result":{"level":"OVERLOADED" if i>=first_fire_idx else "STABLE"}}
            for i in range(n)]

n=2000
print(f"{'first fires at event':>22} | {'reported delay':>14} | interpretation")
print("-"*72)
for idx in (3, 21, 100, 199, 200, 201, 400, 1000):
    d = detection_delay_us(mk(n, idx))
    inside = "INSIDE warm-up -> clipped" if idx < int(n*0.1) else "after warm-up -> real"
    print(f"{idx:>22} | {str(d)+' us':>14} | {inside}")
