Deflaked `TestHandleCall_LocalConcurrentCalls*_WithMux` wall-clock assertions:
the parallel-vs-serialized check now budgets against half the serialized
baseline (1.25s for the 50-caller fan-out instead of 250ms absolute) and
retries the fan-out up to 3 times, so loaded CI runners' scheduling jitter
(observed 602ms in job 202383) can no longer fail a healthy mux while a real
serialization regression still fails deterministically.
