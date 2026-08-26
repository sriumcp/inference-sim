"""Prequential (one-step-ahead) self-normalized CS for a regression slope.

Fixes the under-coverage the plug-in version showed (18.25% vs a 5% budget). The defect:
in-sample residuals satisfy sum(x_i r_i) == 0 exactly by the normal equations, so they
cannot reveal the fit's own error and V understates the true variation.

Prequential construction: at each step n, form the fit from data STRICTLY BEFORE n and
predict v_n. The prediction error is genuinely out-of-sample, hence conditionally centred
given the past -- a real martingale difference. Then

    M_n = sum_i x_i * ehat_i          (martingale, x_i known before step i)
    V_n = sum_i (x_i * ehat_i)^2      (its observed quadratic variation)
    |ghat - g| <= sqrt(V_n * L_n) / S_n,  L_n = 2 log(1/alpha) + log(1 + log(1+n))

Everything is O(1) per step from running sums. No dispersion estimate anywhere: V_n is the
observed variation, so clustered/heavy-tailed errors inflate it on their own.
"""
import math, random

class PrequentialSlopeCS:
    __slots__ = ("n","su","sv","suu","suv","M","V","alpha")
    def __init__(self, alpha):
        self.n=0; self.su=self.sv=self.suu=self.suv=0.0
        self.M=0.0; self.V=0.0; self.alpha=alpha

    def _fit(self):
        n=self.n
        if n<3: return None
        S=self.suu-self.su*self.su/n
        if S<=1e-12: return None
        g=(self.suv-self.su*self.sv/n)/S
        a=(self.sv-g*self.su)/n
        return a,g,S

    def update(self, u, v):
        """Observe (u,v). Accumulates the martingale using the PRE-update fit."""
        f=self._fit()
        if f is not None:
            a,g,_S=f
            ehat=v-(a+g*u)              # one-step-ahead prediction error: out of sample
            x=u-self.su/self.n          # centring uses only past data -> known before step
            self.M+=x*ehat
            self.V+=(x*ehat)**2
        self.n+=1
        self.su+=u; self.sv+=v; self.suu+=u*u; self.suv+=u*v

    def interval(self):
        f=self._fit()
        if f is None or self.V<=0: return None,float("inf")
        _a,g,S=f
        L=2.0*math.log(1.0/self.alpha)+math.log(1.0+math.log(1.0+self.n))
        return g, math.sqrt(self.V*L)/S

def path(n,g0,noise,seed,burst=1.0):
    rng=random.Random(seed); out=[]
    for i in range(1,n+1):
        u=math.log(i+1)
        e=rng.gauss(0,noise)*(burst**2 if rng.random()<1.0/(burst**2) else 1.0)
        out.append((u,g0*u+e))
    return out

alpha,g0=0.05,0.5
print("=== P1 COVERAGE under CONTINUOUS inspection (the property that failed before) ===")
for trials in (400,):
    fails=0
    for t in range(trials):
        cs=PrequentialSlopeCS(alpha); bad=False
        for k,(u,v) in enumerate(path(400,g0,0.30,seed=2000+t),1):
            cs.update(u,v)
            if k>=10 and k%10==0:
                g,w=cs.interval()
                if g is not None and abs(g-g0)>w: bad=True; break
        fails+=bad
    print(f"  path-wise failure: {fails}/{trials} = {fails/trials:.4f}  (budget {alpha})  "
          f"{'PASS' if fails/trials<=alpha else 'FAIL'}")

print()
print("=== P2 SHRINKAGE ===")
cs=PrequentialSlopeCS(alpha); marks={50:None,200:None,800:None,3200:None}
for k,(u,v) in enumerate(path(3200,g0,0.30,seed=11),1):
    cs.update(u,v)
    if k in marks: marks[k]=cs.interval()[1]
prev=None
for k in sorted(marks):
    w=marks[k]; print(f"  n={k:5} halfwidth={w:.4f}"+(f"  {prev/w:5.2f}x tighter" if prev else "")); prev=w

print()
print("=== P3 ADAPTIVITY: wider on bursty errors, with NO dispersion input ===")
for b in (1.0,2.0,4.0):
    ws=[]
    for s in range(30):
        cs=PrequentialSlopeCS(alpha)
        for u,v in path(600,g0,0.30,seed=900+s,burst=b): cs.update(u,v)
        ws.append(cs.interval()[1])
    print(f"  burst={b:>3}  mean halfwidth={sum(ws)/len(ws):.4f}")
