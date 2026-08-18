package sandbox

import "testing"

func TestEncodeDecodeSpecPreservesSkipPull(t *testing.T) {
	spec := sampleSpec()
	spec.SkipPull = true
	raw, err := EncodeSpecChecked(spec)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := DecodeSpec(raw)
	if !ok {
		t.Fatal("DecodeSpec failed")
	}
	if !got.SkipPull {
		t.Fatal("SkipPull was not preserved")
	}
}
