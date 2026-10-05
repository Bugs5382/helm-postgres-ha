package pg

/*
MIT License

Copyright (c) 2026 Shane

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.
*/
import "testing"

func TestJSONListHasEntries(t *testing.T) {
	cases := []struct {
		out  string
		want bool
	}{
		{"[]", false},
		{"[]\n", false},
		{"  []  ", false},
		{"null", false},
		{"", false},
		{`[{"id":1,"parent_id":0,"start_segment":"000000010000000000000001","segments_count":1,"status":"OK"}]`, true},
		{`[{"backup_name":"base_000000010000000000000002","time":"2026-10-05T17:49:00Z"}]`, true},
	}
	for _, c := range cases {
		got, err := jsonListHasEntries(c.out)
		if err != nil || got != c.want {
			t.Errorf("jsonListHasEntries(%q) = %v, %v; want %v", c.out, got, err, c.want)
		}
	}
	if _, err := jsonListHasEntries("INFO: not json"); err == nil {
		t.Error("output that is not a JSON list was accepted")
	}
}
