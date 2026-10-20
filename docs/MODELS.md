# Models

How a model trained in Edge Impulse becomes a SCIoT model directory: the
segments the device and the worker run, and the manifest the server reads.

```text
Edge Impulse Studio ──▶ two exports ──▶ sciot-model build ──▶ <models_root>/<model_key>/
                                                                 submodel_0.tflite … submodel_N.tflite
                                                                 manifest.json
```

The same directory is used by the server (worker) **and** by the devices: see
[Model hash](#model-hash).

## 1. Download the two exports

In the project, **Deployment**, download for the **same model version**:

| Export | Studio option | Example name | Used for |
|---|---|---|---|
| SavedModel | *TensorFlow SavedModel* | `ei-<project>-object-detection-tensorflow-savedmodel-model.41.zip` | graph and weights → the segments |
| C++ library | *C++ library (Linux)*, TensorFlow Lite engine, **unoptimized (float32)** | `<project>-cpp-linux-v8.zip` | post-processing (decoder, labels, thresholds) and Edge Impulse's own `.tflite` for the verification |

From the C++ export only three files are read:

| File | What |
|---|---|
| `model-parameters/model_metadata.h` | `EI_CLASSIFIER_OBJECT_DETECTION_LAST_LAYER` (architecture → decoder), input width/height, `..._OBJECT_DETECTION_THRESHOLD`, `..._OBJECT_DETECTION_COUNT`, `..._QUANTIZATION_ENABLED`, `..._TFLITE_INPUT_DATATYPE` / `..._OUTPUT_DATATYPE`, project id, deploy version, Studio version |
| `model-parameters/model_variables.h` | labels in order (`ei_classifier_inferencing_categories_*`), NMS IoU (`ei_object_detection_nms_*`) |
| `tflite-model/*.tflite` | the whole model as Edge Impulse runs it |

The number in the SavedModel name (`model.41`) and in the `.tflite` name
(`tflite_learn_<project>_41`) is the model version. The build does not trust
names: it checks the content (step 4 below).

## 2. Build

```bash
uv run --project worker sciot-model build \
    ei-sciot---fomo-models-object-detection-tensorflow-savedmodel-model.41.zip \
    --ei-cpp-export sciot-fomo-models-cpp-linux-v8-yolo-small.zip \
    --sample images/stop.png --sample images/bicycle.jpg \
    --output ~/sciot-models/yolo_small_320
```

The steps, each failing with a clear message before the next:

1. **Read the C++ export.** An architecture without a SCIoT decoder, or a
   quantized export, stops here.
2. **Load the SavedModel and check it against the C++ export:** input
   `[1, height, width, 3]` equal to the export's, output columns matching the
   decoder and the number of labels.
3. **Choose the cuts and convert the segments.** A cut is a tensor through
   which exactly one activation is live, so every segment has one input and
   one output. Activations (ReLU, ReLU6, …) are preferred. `--cut TENSOR`
   forces specific cuts, `--max-segments N` keeps N−1 of the automatic ones.
4. **Verify** the whole chain of segments against the SavedModel **and**
   against Edge Impulse's `.tflite`, on a random sample and on every
   `--sample` image. The second comparison proves the two exports are the
   same model. Pass real images: on random noise a detection model's scores
   stay near zero and an error there would go unnoticed.
5. **Write** the directory and print the `model_hash`.

Conversion is float32 without optimizations: the segments compute the same
numbers as the source model (differences ~1e-5).

## 3. The model directory

`manifest.json`, format version 2:

```json
{
  "format_version": 2,
  "quantization": null,
  "tool": {"name": "sciot-model", "tensorflow": "2.20.0"},
  "source": {
    "savedmodel": {"file": "ei-...-model.41.zip", "sha256": "…"},
    "edge_impulse": {"project_id": 1011188, "deploy_version": 8, "studio_version": "1.94.5"}
  },
  "input":  {"shape": [1, 320, 320, 3], "dtype": "float32"},
  "output": {"shape": [1, 2100, 6], "dtype": "float32"},
  "cuts": [{"tensor": "…/activation_188_1/Relu6:0", "operation": "…", "operation_type": "Relu6"}],
  "segments": [{"index": 0, "file": "submodel_0.tflite", "size_bytes": 4972, "sha256": "…",
                "input": {"shape": [1, 320, 320, 3], "dtype": "float32"},
                "output": {"shape": [1, 160, 160, 32], "dtype": "float32"}}],
  "verification": {"samples": 2, "atol": 0.0001, "rtol": 0.0001,
                   "savedmodel": {"max_abs_error": 3.5e-05, "max_rel_error": 1e-05},
                   "edge_impulse": {"max_abs_error": 3.6e-05, "max_rel_error": 1e-05}},
  "postprocess": {"decoder": "yolo_pro", "labels": ["bicycle", "stop sign"],
                  "score_threshold": 0.5, "max_detections": 10, "iou_threshold": 0.2},
  "model_hash": "…"
}
```

- `source` holds file names and hashes, never local paths. `sha256` is empty
  when the SavedModel was given as a directory.
- `quantization` is `null` for float32. A quantized build will describe its
  precision there and add `scale`/`zero_point` to each quantized tensor of
  `segments`; every tensor's `dtype` is already the real one.
- `cuts` keeps the tensor names: a future quantized build reuses them, so that
  the split points are the same in every precision (see [Quantization](#quantization)).
- `postprocess` is what devices receive at registration; labels keep Edge
  Impulse's names exactly (`"stop sign"`, with the space).
- Error `max_rel_error` only counts values with `|expected| ≥ 1e-3`.

## 4. config.json

A model needs only its key and directory: the server reads the segment
count, the input and the post-processing from `manifest.json`, and sends
them to devices at registration (`num_layers`, `last_offloading_layer`,
`input`, `postprocess`).

```json
{"model_key": "yolo_small_320", "tflite_dir": "/home/lab/sciot-models/yolo_small_320",
 "postprocess": {"score_threshold": 0.6}}
```

`postprocess` is optional and overrides only the thresholds:
`score_threshold` and `iou_threshold` in (0, 1], `max_detections` ≥ 1.
`decoder` and `labels` describe the model and cannot be overridden.

At startup the server refuses a model directory when:

- `manifest.json` is missing or has another `format_version`;
- the model is quantized (not supported yet);
- the `*.tflite` files are not exactly the manifest's segments (a leftover
  segment of an older build, a missing one);
- the recomputed `model_hash` differs from the manifest's (files from
  different builds).

`last_offloading_layer` and `input_size_bytes` are no longer accepted in
`config.json`: the first is the number of segments minus one, the second is
replaced by `input`.

Post-processing parameters are read at startup only: to change a threshold,
change it and restart the server; devices register again and receive it.

## Model hash

`model_hash` is the MD5 of the `.tflite` files concatenated in **name** order
(the server, the worker and the clients compute it the same way). Devices
register with it.

> ⚠️ **Rebuilding a model changes its hash.** Converting the same export with
> another TensorFlow version produces different bytes (numerically equal,
> ~1e-5). After a rebuild, replace the files on **every device** with those
> of the same build directory, or registration fails with `model_not_found`.
> `manifest.json` records the TensorFlow version to explain such differences.

The server recomputes the hash at startup and compares it with the
manifest: a directory with segments from two builds is refused.

## Quantization

`sciot-model build` produces **float32 only**. A quantized C++ export
(`EI_CLASSIFIER_QUANTIZATION_ENABLED 1`, or an input/output data type other
than float32) is refused, since Edge Impulse's thresholds would have been
tuned on a model other than the one built.

Quantized builds are planned before the ESP32 client, and nothing in the
format assumes a bit width: TFLite offers int8, uint8, 16-bit activations
with 8-bit weights, float16 weights, and the precision will be a build option.
The intended scenario is mixed: the device runs quantized segments `0..k`,
the server runs float32 segments `k+1..last`. This needs:

- the **same cuts** in every precision: a quantized build reuses `cuts` of
  the float32 manifest;
- each quantized cut tensor described in the manifest (`dtype`, `scale`,
  `zero_point`);
- the frame `encoding` naming the tensor's data type, so that the server
  dequantizes it (`x = scale · (q − zero_point)`) before running its segments;
- the server registry accepting several `model_hash` values (one per
  precision) for the same model.

## Troubleshooting

| Error | Cause |
|---|---|
| `unsupported last layer …` | the architecture has no SCIoT decoder: see [Adding a decoder family](#adding-a-decoder-family) |
| `… has no EI_CLASSIFIER_…` | Edge Impulse renamed a macro in a newer Studio version: update `build/edge_impulse.py` |
| `segments differ from the Edge Impulse .tflite … same model version?` | the two exports come from different model versions: download both again |
| `segments differ from the SavedModel` | a cut or the conversion is wrong: report it with the model |
| `no single-tensor cut found` | the graph branches from input to output; try `--cut` with a tensor where the branches join |

## Adding a decoder family

A family (FOMO, YOLO-Pro, …) is a decoding algorithm. A new model of a
supported family needs no code; a new family needs:

1. the macro → decoder entry in `build/edge_impulse.py` (`DECODERS`) and its
   output-shape rule in `build/cli.py` (`check_consistency`);
2. the decoder in the worker, the Python client and the ESP32 client;
3. golden files: a real output tensor and the expected detections, decoded
   identically by all three.

The decoder specifications and the golden file format are added with the
decoders (phase 2.4e).
