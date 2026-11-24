export function stats(values) {
  if (!values.length || values.some((value) => !Number.isSafeInteger(value) || value <= 0)) throw new Error("Invalid metric samples.");
  const sorted = [...values].sort((a, b) => a - b);
  const percentile = (fraction) => sorted[Math.ceil(fraction * sorted.length) - 1];
  return { count: values.length, p50_ns: percentile(.5), p95_ns: percentile(.95) };
}

export function verdict(parent, peer) {
  const regressions = Object.fromEntries(["overall", "steer", "follow_up"].map((flow) => {
    const base = parent.parent[flow].p95_ns;
    const head = parent.head[flow].p95_ns;
    return [flow, head * 5 > base * 6 && head - base > 50_000_000];
  }));
  const parentRegression = Object.values(regressions).some(Boolean);
  const peerProven = peer.acceptance.p95_ns < 100_000_000 && peer.delivery.p95_ns < 1_000_000_000;
  return { status: parentRegression ? "regressed" : peerProven ? "passed" : "inconclusive", regressions, parentRegression, peerProven };
}
