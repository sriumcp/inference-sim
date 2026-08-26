# Correction: the incumbents are NOT blind to T2-IN

**Retracts a claim made in three earlier commits and two findings documents.**

## What I claimed

That all three incumbent detectors (composite, threshold, backlog-drift) are BLIND
to T2-IN -- that a 32x prompt increase leaves every one of them STABLE -- and that
this was the primary justification for a new detector carrying a prefill term.

## What is actually true

Measured on the real simulator, with each detector FPR-calibrated first per §3.4,
on the T2-IN ladder (rate held at 0.6x nominal, only prompt scaled):

| detector | T1 | T2-IN | T2-OUT | T4 | FPR | regret |
|---|---|---|---|---|---|---|
| **randomwalk** (peak_over_elapsed) | PASS | PASS | PASS | PASS | 0.0 | **10.0** |
| composite | PASS | **PASS** | PASS | PASS | 0.0 | **10.0** |
| threshold | PASS | **PASS** | PASS | PASS | 0.0 | **10.0** |
| backlog-drift | FAIL | PASS | FAIL | FAIL | 0.25 | 340.0 |

composite and threshold both PASS T2-IN, with exactly the right shape: silent at
the 1x base rung, fired at the 8x rung. **My candidate ties them; it does not beat
them.**

## Why my earlier measurement was wrong

The blindness claim came from a UNIT TEST over hand-built events, not from the
simulator. Its stream builder is:

```go
buildStream(n, gapUs, in, out, svcUs)   // svcUs INDEPENDENT of in/out
buildStream(400, 100_000, 4096, 128, 20_000)  // stressed
buildStream(400, 100_000,  128, 128, 20_000)  // base -- SAME svcUs
```

The service time is a fixed parameter, identical in both legs. So a 32x prompt
increase cost ZERO extra latency in that stream. On the real simulator an 8x prompt
at fixed rate raises mean E2E **25.9x** (6,245 ms -> 161,682 ms), because prefill is
real work.

A latency-based detector is therefore SUPPOSED to fire on a prompt stressor, and it
does. My test asserted that prompts are free and then reported detectors as blind to
a stressor that, in that stream, genuinely cost nothing. The apparatus was
physically incoherent -- the same class of error as the T3 factor-B trap and the
Lindley-coherence bug caught earlier in this work, and the third time an
author-built synthetic stream has produced a false conclusion here.

## What the unit test can legitimately show

The ABLATION, which is a claim about the work-drift statistic and not about other
detectors: with `kappa > 0` the residual responds to a prompt-only change; with
`kappa = 0` it is blind (Proposition 4). That is a property of the statistic, valid
even in an incoherent stream, because both legs share the same stream. The test is
retargeted to exactly that and renamed
`TestMetamorphic_T2IN_KappaTermIsTheMechanism`, with the builder's limitation
documented at its definition so the next reader cannot repeat the mistake.

## Consequence for the campaign

**The justification for a new detector is now weaker and must be stated as such.**
The drafts' own bar (`detection_strategies.md` §3b) is explicit: *"merely matching
the §2b level/symptom detectors is not enough"*. On the 4-test suite at matched FPR,
randomwalk MATCHES composite and threshold at regret 10.0. It does not yet clear
that bar.

What remains genuinely true and unaffected:

- **backlog-drift is broken** (regret 340, fails T1/T2-OUT/T4, FPR 0.25 -- it cannot
  even hold the false-alarm budget). The misimplementation diagnosed at the start of
  this work is confirmed empirically.
- **The horizon finding stands**: R_t needs a long horizon, and at n=6000 it produces
  a clean step function at the cliff.
- **The gaming guards stand**: a 100%-FPR configuration scoring the best regret on
  the board was correctly rejected by two independent constraints.

What the campaign must now settle is whether ANY point in the 1200-point space beats
the incumbents on LEAD TIME at matched FPR -- which is the metric §1a calls primary
and the one the tie at regret 10.0 does not resolve. The current regret function
weights failed tests at 100 and lead time at 10, so a tie on passes makes lead time
the tiebreak; all three tied detectors report lead_time_mult = -0.5 in quick mode,
so this needs the full-resolution ladder to separate.
