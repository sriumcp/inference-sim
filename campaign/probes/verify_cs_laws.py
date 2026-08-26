"""BEHAVIORAL laws the CS must satisfy. No internals touched -- only inputs and verdicts.

Each test would survive a complete rewrite of the estimator, and each can FAIL: the
plug-in version passes L2/L3/L5 and fails L1, which is exactly the discrimination wanted.
"""
import math, random, sys
sys.path.insert(0, "/Users/sri/Documents/Projects/inference-sim/.worktrees/anytime-campaign/campaign/probes")
from verify_prequential_cs import PrequentialSlopeCS, path

FAILS=[]
def law(name, ok, detail=""):
    print(f"  {'PASS' if ok else 'FAIL'}  {name}" + (f"   {detail}" if detail else ""))
    if not ok: FAILS.append(name)

alpha=0.05
print("=== L1 COVERAGE: the guarantee itself, under continuous inspection ===")
for g0 in (0.0, 0.5, 1.0):
    fails=0; trials=200
    for t in range(trials):
        cs=PrequentialSlopeCS(alpha); bad=False
        for k,(u,v) in enumerate(path(300,g0,0.30,seed=t*7+int(g0*10)),1):
            cs.update(u,v)
            if k>=10 and k%10==0:
                g,w=cs.interval()
                if g is not None and abs(g-g0)>w: bad=True; break
        fails+=bad
    law(f"covers true slope g={g0}", fails/trials<=alpha*1.5, f"failure {fails/trials:.3f} vs {alpha}")

print()
print("=== L2 DECIDABILITY: a clearly-overloaded path must eventually EXCLUDE criticality ===")
# g0=1.0 is overloaded; the interval must end up wholly above the 0.5 boundary
cs=PrequentialSlopeCS(alpha); decided_at=None
for k,(u,v) in enumerate(path(2000,1.0,0.30,seed=3),1):
    cs.update(u,v); g,w=cs.interval()
    if g is not None and g-w>0.5: decided_at=k; break
law("overloaded path commits above 0.5", decided_at is not None, f"at n={decided_at}")

print()
print("=== L3 SYMMETRY: a clearly-healthy path must eventually exclude criticality BELOW ===")
cs=PrequentialSlopeCS(alpha); decided_at=None
for k,(u,v) in enumerate(path(2000,0.0,0.30,seed=4),1):
    cs.update(u,v); g,w=cs.interval()
    if g is not None and g+w<0.5: decided_at=k; break
law("healthy path commits below 0.5", decided_at is not None, f"at n={decided_at}")

print()
print("=== L4 BURSTINESS COSTS EVIDENCE: same true slope, burstier => decides LATER ===")
res={}
for b in (1.0,2.0,4.0):
    ns=[]
    for s in range(20):
        cs=PrequentialSlopeCS(alpha); d=None
        for k,(u,v) in enumerate(path(4000,1.0,0.30,seed=60+s,burst=b),1):
            cs.update(u,v); g,w=cs.interval()
            if g is not None and g-w>0.5: d=k; break
        ns.append(d if d else 4000)
    res[b]=sum(ns)/len(ns)
    print(f"    burst={b:>3}  mean n-to-decide={res[b]:7.1f}")
law("burstier needs more observations", res[1.0]<res[2.0]<res[4.0],
    f"{res[1.0]:.0f} < {res[2.0]:.0f} < {res[4.0]:.0f}")

print()
print("=== L5 NO FREE LUNCH: tighter alpha must never decide EARLIER on the same data ===")
data=path(4000,1.0,0.30,seed=5)
ds={}
for a in (0.10,0.01):
    cs=PrequentialSlopeCS(a); d=None
    for k,(u,v) in enumerate(data,1):
        cs.update(u,v); g,w=cs.interval()
        if g is not None and g-w>0.5: d=k; break
    ds[a]=d
law("alpha=0.01 no earlier than alpha=0.10", ds[0.01]>=ds[0.10], f"{ds[0.01]} >= {ds[0.10]}")

print()
print("=== L6 DISCRIMINATION: does the OLD plug-in version FAIL L1? (proves L1 can fail) ===")
def plugin_halfwidth(us,vs,alpha):
    n=len(us); ub=sum(us)/n; vb=sum(vs)/n
    xs=[u-ub for u in us]; S=sum(x*x for x in xs)
    if n<3 or S<=0: return None,float('inf')
    g=sum(x*(v-vb) for x,v in zip(xs,vs))/S; a=vb-g*ub
    r=[v-(a+g*u) for u,v in zip(us,vs)]
    V=sum((x*ri)**2 for x,ri in zip(xs,r))
    L=2*math.log(1/alpha)+math.log(1+math.log(1+n))
    return g, math.sqrt(max(V,1e-300)*L)/S
fails=0
for t in range(200):
    d=path(300,0.5,0.30,seed=2000+t); bad=False
    for n in range(10,301,10):
        g,w=plugin_halfwidth([x[0] for x in d[:n]],[x[1] for x in d[:n]],alpha)
        if g is not None and abs(g-0.5)>w: bad=True; break
    fails+=bad
law("plug-in version VIOLATES coverage (so L1 is discriminating)", fails/200>alpha*2,
    f"plug-in failure {fails/200:.3f} vs prequential 0.040")

print()
print(("ALL LAWS PASS" if not FAILS else f"FAILED: {FAILS}"))
