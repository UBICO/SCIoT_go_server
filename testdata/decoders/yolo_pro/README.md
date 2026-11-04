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

**Real** files (`real_*.json` + `real_*.bin`): the output of yolo-small on
an image, written by `worker/scripts/make_decoder_golden.py`. The `.bin` is
the raw output tensor (float32, little-endian, shape and sha256 in the
`.json` under `"output"`); the image is not stored. Their expected
`detections` come from the worker's decoder: the worker is the reference that
the Python client and the ESP32 must match on real data, with dozens of
overlapping candidates.

| File | Image | Checks |
|---|---|---|
| `real_stop` | `client/sciot_client/stop.png` | 19 rows above the threshold reduced by NMS to one stop sign |
| `real_stop_and_bicycle` | COCO 000000004355 | two classes in one image |
| `real_empty` | COCO 000000000531 | no object |

A test decodes `rows` (or the `.bin`) with `postprocess` and compares with
`detections`: same length and order, same labels, scores within 1e-6, box
values within 1e-5.
