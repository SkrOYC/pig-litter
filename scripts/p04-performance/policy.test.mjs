import { expect, test } from "bun:test";
import { stats, verdict } from "./policy.mjs";

const ns = (ms) => ms * 1_000_000;
const parent = (base, head) => ({
  parent: Object.fromEntries(["overall", "steer", "follow_up"].map((flow) => [flow, { p95_ns: ns(base) }])),
  head: Object.fromEntries(["overall", "steer", "follow_up"].map((flow) => [flow, { p95_ns: ns(head) }])),
});
const peer = (acceptance, delivery) => ({ acceptance: { p95_ns: ns(acceptance) }, delivery: { p95_ns: ns(delivery) } });

test("nearest-rank percentiles retain slow samples", () => {
  expect(stats([...Array(19).fill(ns(10)), ns(90)])).toEqual({ count: 20, p50_ns: ns(10), p95_ns: ns(10) });
  expect(stats([...Array(18).fill(ns(10)), ns(80), ns(90)]).p95_ns).toBe(ns(80));
  expect(() => stats([0])).toThrow();
});

test("parent fails only when both regression limits are exceeded", () => {
  expect(verdict(parent(250, 300), peer(99, 999)).status).toBe("passed");
  expect(verdict(parent(300, 351), peer(99, 999)).status).toBe("passed");
  expect(verdict(parent(100, 151), peer(99, 999)).status).toBe("regressed");
  const split = parent(100, 100);
  split.head.steer.p95_ns = ns(151);
  expect(verdict(split, peer(99, 999)).regressions.steer).toBe(true);
});

test("unproven acceptance and delivery bounds remain inconclusive", () => {
  expect(verdict(parent(100, 100), peer(100, 999)).status).toBe("inconclusive");
  expect(verdict(parent(100, 100), peer(99, 1000)).status).toBe("inconclusive");
  expect(verdict(parent(100, 151), peer(100, 1000)).status).toBe("regressed");
});
