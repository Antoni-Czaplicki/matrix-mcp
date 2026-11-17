package main

import "testing"

func TestFinalAnswer(t *testing.T) {
	cases := map[string]string{
		"blah\nANSWER: Jan Kowalski":          "Jan Kowalski",
		"**ANSWER:** `example.com`":           "example.com",
		"ANSWER: first\nmore\nANSWER: second": "second",
		"no marker\nthe last line":            "the last line",
	}
	for in, want := range cases {
		if got := finalAnswer(in); got != want {
			t.Errorf("finalAnswer(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGrade(t *testing.T) {
	task := Task{Answers: []string{"Łucja Wiśniewska"}}
	if !grade(task, "It was lucja wisniewska.") {
		t.Error("contains match should ignore case, diacritics and punctuation")
	}
	if grade(task, "Jan") {
		t.Error("wrong answer passed")
	}
	if !grade(Task{Answers: []string{"^\\d{2}:\\d{2}$"}, Match: "regex"}, "18:30") {
		t.Error("regex match failed")
	}
	if grade(Task{Answers: []string{"3"}, Match: "exact"}, "13") {
		t.Error("exact match should not accept substrings")
	}
}
