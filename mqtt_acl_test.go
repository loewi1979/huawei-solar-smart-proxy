package main

import "testing"

func TestExplorerSubscriptionFilters(t *testing.T) {
	h := subscriberAuth{cfg: MQTTConfig{Topic: "huawei_wechselrichter"}}
	for _, topic := range []string{"#", "$SYS/#", "huawei_wechselrichter/#", "huawei_wechselrichter", "huawei_wechselrichter/status"} {
		if !h.OnACLCheck(nil, topic, false) {
			t.Fatalf("read subscription rejected: %s", topic)
		}
		if h.OnACLCheck(nil, topic, true) {
			t.Fatalf("remote publishing allowed: %s", topic)
		}
	}
	for _, topic := range []string{"unrelated/private", "$SYS/broker/version", "huawei_wechselrichter/other"} {
		if h.OnACLCheck(nil, topic, false) || h.OnACLCheck(nil, topic, true) {
			t.Fatalf("unrelated topic permitted: %s", topic)
		}
	}
}
