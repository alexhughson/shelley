package lazycue

import "testing"

func TestStepSummaryNamesTheTab(t *testing.T) {
	cases := map[Step]string{
		{Action: ActionClick, Selector: "#send"}:           "click #send",
		{Action: ActionClick, Selector: "#send", Tab: "B"}: "[B] click #send",
		{Action: ActionCloseTab, Tab: "B"}:                 "[B] close_tab",
		{Action: ActionNavigate, Tab: "B"}:                 "[B] navigate to the first tab's page",
	}
	for step, want := range cases {
		if got := StepSummary(step); got != want {
			t.Errorf("StepSummary(%+v) = %q, want %q", step, got, want)
		}
	}
}
