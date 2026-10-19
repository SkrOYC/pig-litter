# Terminal performance checks

The common widget workload uses the same byte-identical benchmark fixture at baseline `6888fa6` and candidate 3. It contains four named children, three Unicode or unnamed children, and ten children spanning every state. Each invocation projects those inputs at 32 or 100 columns. The fixture SHA256 is `91bd0f76d77975f3afd334b249118dcf2cab11a0c44e9ad731728135bd843ee8`.

Both runs use Go 1.27.1, CGO enabled, Linux amd64, the same Ryzen 7 PRO 4750U, `GOMAXPROCS=1`, five samples per case and 300 ms per sample. The table gives medians from the [baseline output](common-widget-baseline.txt) and [final head output](common-widget-head.txt).

| Input | Columns | Baseline, µs/op | Head, µs/op | Head / baseline |
| --- | ---: | ---: | ---: | ---: |
| Named | 32 | 0.522 | 3.883 | 7.4 |
| Named | 100 | 0.521 | 2.898 | 5.6 |
| Unicode | 32 | 3.560 | 9.576 | 2.7 |
| Unicode | 100 | 3.369 | 9.707 | 2.9 |
| Every state | 32 | 1.595 | 4.505 | 2.8 |
| Every state | 100 | 1.398 | 3.201 | 2.3 |

The head does more display work. It sanitizes human text, counts retained states, selects active work before recent settled work and uses terminal width. Its largest median is 9.707 µs, below the predeclared 500 µs projection budget. This comparison measures the same inputs, not identical output. It excludes SDK transport, terminal repaint and the larger terminal verifier's total runtime.

The measured head `widget.go` SHA256 is `48624873d29639100bd90f5599bb169776404e3796a2686c74188ef7f9307ae2`, matching candidate 3. The earlier head measurement preceded the recent-settled selection correction and remains in local history; this final measurement supersedes it for delivery.

The common command is run in an archived or copied module with the same fixture inserted.

```sh
go test -mod=readonly -run '^$' -bench '^BenchmarkCommonWidget$' \
  -benchmem -benchtime=300ms -count=5 -cpu=1 .
```

The final head also projects 20,000 retained records, including two active records, with constant two-slot selection storage. The [three samples](large-history.txt) at 32 columns are 140.593, 137.555 and 137.380 µs/op. The median is 137.555 µs, with 648 B/op and 34 allocations, below the same 500 µs budget. This checks retained-history scale without imposing a product record limit.

```sh
go test -mod=readonly -run '^$' -bench '^BenchmarkWidgetLargeRetainedHistory$' \
  -benchmem -benchtime=300ms -count=3 -cpu=1 .
```

Native key-to-visible-state results are added after their final gate. Native panel controls have a separate two-second budget under the fixed deterministic fixture. Total verifier duration is not a baseline/head performance metric.
