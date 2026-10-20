# YOLO-Pro decoder golden files

Executable specification of the `yolo_pro` decoder (docs/MODELS.md,
"Decoders"). Every implementation (worker, Python client, ESP32) must pass
all of them.

**Synthetic** files (`*.json` without a `.bin`): a few hand-written rows, one
rule each, stated in `"rule"`. The expected `detections` are derived from the
rule, not produced by any decoder. Values are exact in binary (multiples of
1/8) so that float32 and float64 implementations agree exactly.

Each file:

| Key | Content |
|---|---|
| `rule` | what the file checks, in one sentence |
| `postprocess` | the decoder parameters, as the server sends them |
| `columns` | meaning of each row value |
| `rows` | the model output, one row per candidate box |
| `detections` | the expected result, in order |

A test decodes `rows` with `postprocess` and compares with `detections`:
same length and order, same labels, scores within 1e-6, box values within 1e-5.
