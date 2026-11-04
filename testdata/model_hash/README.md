# model_hash

The executable specification of the model hash (docs/MODELS.md, "Model
hash"): the MD5 of the `*.tflite` files concatenated in **name** order.
`submodel_10.tflite` comes before `submodel_2.tflite`, so the hash is
`md5("abdc")`, not the execution order `md5("abcd")`; `notes.txt` is not a
segment and is ignored.

`expected_hash` is checked by every implementation: the Go registry, the
worker and the Python client. A change of rule changes this file and all of
them.
