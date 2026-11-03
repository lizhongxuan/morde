package project

import "testing"

func TestWorkItemBlockResumeAndDeliveryLifecycle(t *testing.T) {
	item := WorkItem{
		ID: "j1234567", ProjectID: "j7654321", Title: "交付报告", AssigneeID: "u1234567",
		State: WorkItemOpen, Version: 1,
	}
	taskID := "q1234567"
	linked, err := item.LinkTask(taskID)
	if err != nil {
		t.Fatal(err)
	}
	linked, err = linked.LinkTask(taskID)
	if err != nil || len(linked.TaskIDs) != 1 {
		t.Fatalf("repeated task link = %#v, %v", linked.TaskIDs, err)
	}
	blocked, err := linked.Block("等待成员确认")
	if err != nil || blocked.State != WorkItemBlocked || blocked.BlockedReason == "" {
		t.Fatalf("block = %#v, %v", blocked, err)
	}
	if _, err := blocked.Deliver(WorkItemDelivery{TaskID: taskID, WorkspaceID: "w1234567", Revision: 3}); err == nil {
		t.Fatal("blocked work item was delivered without resuming")
	}
	resumed, err := blocked.Resume()
	if err != nil || resumed.State != WorkItemOpen || resumed.BlockedReason != "" {
		t.Fatalf("resume = %#v, %v", resumed, err)
	}
	delivered, err := resumed.Deliver(WorkItemDelivery{TaskID: taskID, WorkspaceID: "w1234567", Revision: 3})
	if err != nil || delivered.State != WorkItemDelivered || delivered.Validate() != nil {
		t.Fatalf("deliver = %#v, %v", delivered, err)
	}
	if _, err := delivered.Block("later"); err == nil {
		t.Fatal("delivered work item was reopened")
	}
}

func TestWorkItemRequiresLinkedTaskForDeliveryEvidence(t *testing.T) {
	item := WorkItem{
		ID: "j1234567", ProjectID: "j7654321", Title: "交付报告", AssigneeID: "u1234567",
		State: WorkItemOpen, Version: 1,
	}
	if _, err := item.Deliver(WorkItemDelivery{TaskID: "q1234567", WorkspaceID: "w1234567", Revision: 1}); err == nil {
		t.Fatal("unlinked task delivery was accepted")
	}
	if _, err := item.Block(" "); err == nil {
		t.Fatal("empty blocker was accepted")
	}
}
