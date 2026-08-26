# Are threshold and backlog-drift wired the way they were meant to be used?

**Asked by the user after both scored 0.000 correctness.** A detector scoring zero is either a
real result or a harness bug, and the two are indistinguishable without checking. Audited both.

## backlog-drift: MY WIRING WAS WRONG, now fixed

Its defaults require **60-second windows with `MinWindows=5`, plus 2 warm-up and 1 tail window** --
8 complete windows, i.e. **420 seconds of SIMULATED time** before it can classify at all.

A ladder run of 800 requests at the measured cliff (88 rps) spans about **9 seconds of simulated
time**. That is **2.2% of one requirement**. At defaults the detector could never classify, so its
0.000 measured nothing about the detector and everything about my harness.

Fixed: the window is now derived from the run's simulated span so that
`MinWindows + WarmupWindows + TailWindows` complete windows fit inside it -- the configuration the
detector is documented to need. Its `slope_k` calibration knob is untouched and remains its
false-alarm dial. The cached static reference was invalidated so it is re-measured under the fix.

**Verified it now runs:** it produces verdicts at every rung instead of nothing.

## backlog-drift's remaining weakness is DOCUMENTED, not mine

With the window fixed it still reports STABLE at 2x the cliff, and `slope_k` is inert across its
entire grid (0.5 through 50.0 -- every value gives STABLE at 176 rps).

That is the project's own documented degeneracy, from `CLAUDE.md`:

> *"a straight-line fit to backlog is degenerate at exactly rho ~= 1 (backlog grows like sqrt(t),
> so the slope tends to ZERO and the detector reports STABLE at criticality -- the worst failure
> direction)"*

So the distinction that matters: **the horizon was my bug; the insensitivity is the detector's
known structural weakness.** Reporting the second is legitimate; reporting the first would have
been measuring my own harness and calling it a comparison.

Note this also means backlog-drift's `knob_at_grid_edge` constraint violation is real and
meaningful -- an inert knob pinned at its grid's edge is exactly what that guard exists to catch.

## threshold: CORRECTLY WIRED, and its zero is a real result

Measured mean end-to-end latency across the ladder, same traffic:

| rate | x cliff | mean E2E |
|---|---|---|
| 26 | 0.30 | 1096 ms |
| 53 | 0.60 | 1387 ms |
| 88 | 1.00 | 2013 ms |
| 106 | 1.20 | 2438 ms |
| 176 | 2.00 | 3625 ms |

The calibration grid spans 1000-35000 ms, so it brackets the real range, and the walk selected
**2500 ms** -- the smallest knob holding the false-alarm budget on the 0.3-0.6x band. That is a
legitimate calibration, not a mis-set constant: at 2000 ms it would fire on the healthy band.

Its low correctness is therefore genuine detector behaviour: **an absolute-latency rule cannot
separate a 2013 ms cliff from a 2438 ms overload** when its budget forces the knob above both.
That is the structural point the anytime detector's dimensionless boundary is designed to avoid,
and it is fair to report -- the knob is interior, the grid brackets the range, and the calibration
band is the same one every detector gets.

## The general rule this earns

**A detector scoring zero must be audited before it is reported.** Two candidate explanations --
"the detector is weak here" and "I configured it outside its operating range" -- produce identical
numbers, and only one is a finding. The check is cheap: read what the detector's own config
requires, compare it against what the run actually provides, and confirm its knob moves the
verdict somewhere in the grid.

Applied here it found one genuine harness bug (backlog-drift's horizon) and confirmed one genuine
result (threshold's absolute-scale limitation). Without it I would have published both as
comparisons.
