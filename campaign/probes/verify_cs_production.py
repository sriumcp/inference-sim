"""Production-grade CS: adds the RUNNING-INTRINSIC-TIME floor, and tests it behaviorally.

THE DEFECT THIS FIXES. The bare prequential bound decided at n=5, then un-decided at n=6,
8, 9, 10 -- flapping. Cause: V is a martingale variance with only 1-2 increments that early,
so V ~ 6.7e-03 and the interval is spuriously narrow. Coverage still held (4% vs 5%), so the
bound is not WRONG; it is just uninformative at tiny n while looking confident.

THE FIX, and it is principled rather than a magic minimum-n: mix in a PRIOR quadratic
variation V0, i.e. use (V + V0) in place of V. This is the standard Robbins-mixture /
"running intrinsic time" device -- it is what makes a mixture martingale a martingale from
step ONE rather than asymptotically. Two consequences, both desirable:

  * at small n, V0 dominates so the interval is honestly wide;
  * at large n, V dominates so nothing is given up asymptotically.

V0 has a real interpretation and is therefore SETTABLE rather than tuned: it is the
quadratic variation the analyst is willing to assume before seeing data, i.e. one
observation's worth at the expected noise scale. It is a factor in the campaign, not a
constant I picked.
"""
import math, random

class ProductionSlopeCS:
    __slots__=("n","su","sv","suu","suv","M","V","alpha","kappa","sigma0")
    def __init__(self, alpha, kappa, sigma0=0.30):
        self.n=0; self.su=self.sv=self.suu=self.suv=0.0
        self.M=0.0; self.V=0.0; self.alpha=alpha
        self.kappa=kappa; self.sigma0=sigma0
    def _fit(self):
        n=self.n
        if n<3: return None
        S=self.suu-self.su*self.su/n
        if S<=1e-12: return None
        g=(self.suv-self.su*self.sv/n)/S
        return (self.sv-g*self.su)/n, g, S
    def update(self,u,v):
        f=self._fit()
        if f is not None:
            a,g,_=f
            e=v-(a+g*u); x=u-self.su/self.n
            self.M+=x*e; self.V+=(x*e)**2
        self.n+=1; self.su+=u; self.sv+=v; self.suu+=u*u; self.suv+=u*v
    def interval(self):
        f=self._fit()
        if f is None: return None,float("inf")
        _a,g,S=f
        L=2.0*math.log(1.0/self.alpha)+math.log(1.0+math.log(1.0+self.n))
        # V0 = kappa * sigma0^2 * S: expressed in units of S so it is SCALE-FREE and
        # shrinks as the design gains leverage. At V=0 the halfwidth is
        # sqrt(kappa*sigma0^2*L/S), finite from step one and vanishing as S grows.
        V0=self.kappa*self.sigma0*self.sigma0*S
        return g, math.sqrt((self.V+V0)*L)/S

def path(n,g0,noise,seed,burst=1.0):
    rng=random.Random(seed); out=[]
    for i in range(1,n+1):
        u=math.log(i+1)
        e=rng.gauss(0,noise)*(burst**2 if rng.random()<1.0/(burst**2) else 1.0)
        out.append((u,g0*u+e))
    return out

def n_to_decide(g0,alpha,kappa,seed,side="above",cap=6000,burst=1.0):
    cs=ProductionSlopeCS(alpha,kappa)
    for k,(u,v) in enumerate(path(cap,g0,0.30,seed=seed,burst=burst),1):
        cs.update(u,v); g,w=cs.interval()
        if g is None: continue
        if side=="above" and g-w>0.5: return k
        if side=="below" and g+w<0.5: return k
    return None

FAILS=[]
def law(name,ok,detail=""):
    print(f"  {'PASS' if ok else 'FAIL'}  {name}"+(f"   {detail}" if detail else ""))
    if not ok: FAILS.append(name)

alpha, KAPPA = 0.05, 5.0
print("=== L1 COVERAGE still holds with the prior variation ===")
for g0 in (0.0,0.5,1.0):
    fails=0; trials=200
    for t in range(trials):
        cs=ProductionSlopeCS(alpha,KAPPA); bad=False
        for k,(u,v) in enumerate(path(300,g0,0.30,seed=t*13+int(g0*10)),1):
            cs.update(u,v)
            if k>=5:
                g,w=cs.interval()
                if g is not None and abs(g-g0)>w: bad=True; break
        fails+=bad
    law(f"covers g={g0}", fails/trials<=alpha*1.5, f"failure {fails/trials:.3f}")

print()
print("=== L2 NO SPURIOUS DECISION ON A NULL PATH (the law that matters) ===")
# The real risk is not "decided early", it is "decided WRONG". On a critical path
# (g0 = 0.5, exactly the boundary) the detector must not commit either way at a rate
# above alpha -- that is the false-decision rate, and it is what an operator feels.
wrong=0; trials=200
for t in range(trials):
    cs=ProductionSlopeCS(alpha,KAPPA); bad=False
    for k,(u,v) in enumerate(path(400,0.5,0.30,seed=3000+t),1):
        cs.update(u,v); g,w=cs.interval()
        if g is None: continue
        if g-w>0.5 or g+w<0.5: bad=True; break   # committed either way on a boundary path
    wrong+=bad
law("does not commit on a critical (g=0.5) path", wrong/trials<=alpha*1.5,
    f"false-decision rate {wrong/trials:.3f} vs {alpha}")

print("=== L3 RETRACTION IS BOUNDED (not forbidden -- a CS may legitimately retract) ===")
# Demanding zero retractions would be demanding a LATCH, and epoch 6 showed a latch makes
# the temporal-consistency test measure the latch instead of the statistic. The honest law:
# the raw sequence may retract, but retractions must DIE OUT as evidence accumulates.
early_tot=late_tot=0
for s_ in range(30):
    cs=ProductionSlopeCS(alpha,KAPPA); prev=None
    for k,(u,v) in enumerate(path(1500,1.0,0.30,seed=200+s_),1):
        cs.update(u,v); g,w=cs.interval()
        if g is None: continue
        up=(g-w>0.5)
        if prev is True and up is False:
            if k<=300: early_tot+=1
            else: late_tot+=1
        prev=up
law("retractions die out as evidence accumulates", late_tot<=early_tot,
    f"retractions: {early_tot} in n<=300, {late_tot} in n>300")

print("=== L4 BURSTINESS COSTS EVIDENCE (monotone, untold) ===")
means={}
for b in (1.0,2.0,4.0):
    v=[n_to_decide(1.0,alpha,KAPPA,seed=400+s,burst=b) or 6000 for s in range(25)]
    means[b]=sum(v)/len(v); print(f"    burst={b:>3}  mean n={means[b]:7.1f}")
law("burstier => later", means[1.0]<means[2.0]<means[4.0], f"{means[1.0]:.0f} < {means[2.0]:.0f} < {means[4.0]:.0f}")

print()
print("=== L5 NO FREE LUNCH in alpha ===")
a10=[n_to_decide(1.0,0.10,KAPPA,seed=700+s) or 6000 for s in range(25)]
a01=[n_to_decide(1.0,0.01,KAPPA,seed=700+s) or 6000 for s in range(25)]
law("tighter alpha never earlier, path by path", all(x>=y for x,y in zip(a01,a10)),
    f"mean {sum(a01)/25:.0f} vs {sum(a10)/25:.0f}")

print()
print("=== L6 CRITICALITY IS HARDEST: rho~1 must take longer than clear overload ===")
clear=[n_to_decide(1.0,alpha,KAPPA,seed=800+s) or 6000 for s in range(25)]
crit =[n_to_decide(0.6,alpha,KAPPA,seed=800+s) or 6000 for s in range(25)]
law("near-critical slower than clear overload", sum(crit)/25>sum(clear)/25,
    f"crit {sum(crit)/25:.0f} vs clear {sum(clear)/25:.0f}")

print()
print("ALL LAWS PASS" if not FAILS else f"FAILED: {FAILS}")
