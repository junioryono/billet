package retirement

import (
	"encoding/hex"
	"fmt"
)

// NodeConfigVerdict is a current read-only observation bound to exact inputs.
// It carries no completed postcondition and grants no successful-drain proof.
type NodeConfigVerdict struct {
	Schema           int     `json:"schema"`
	Purpose          string  `json:"purpose"`
	Outcome          string  `json:"outcome"`
	Run              string  `json:"run"`
	Guard            string  `json:"guard"`
	Retiring         string  `json:"retiring"`
	Deployment       string  `json:"deployment"`
	TransitionID     string  `json:"transition_id"`
	Variant          Variant `json:"variant"`
	RenderingSHA256  string  `json:"rendering_sha256"`
	OperationsSHA256 string  `json:"operations_sha256"`
	NodeActivity     string  `json:"node_activity"`
	State            string  `json:"state"`
}

// DecodeNodeConfigVerdict requires the exact invocation and both byte digests.
func DecodeNodeConfigVerdict(raw []byte, exitCode int, expected NodeConfigVerdict) (NodeConfigVerdict, error) {
	var verdict NodeConfigVerdict
	if len(raw) > 16<<10 {
		return verdict, fmt.Errorf("node-config verdict exceeds the read bound")
	}
	if err := DecodeDocument(raw, &verdict); err != nil {
		return verdict, err
	}
	if exitCode != 0 || verdict.Schema != 1 || verdict.Purpose != "node-config" || verdict.Outcome != "admitted" ||
		verdict.Variant != VariantRetainedNode || verdict.State != "nothing" ||
		(verdict.NodeActivity != "active" && verdict.NodeActivity != "quiet-inactive" && verdict.NodeActivity != "quiet-failed") {
		return verdict, fmt.Errorf("not an admitted node-config observation")
	}
	expected.Schema, expected.Purpose, expected.Outcome = 1, "node-config", "admitted"
	expected.Variant, expected.State, expected.NodeActivity = VariantRetainedNode, "nothing", verdict.NodeActivity
	if verdict != expected || verdict.Run == "" || verdict.Retiring == "" || !transitionIDPattern.MatchString(verdict.Guard) ||
		!transitionIDPattern.MatchString(verdict.TransitionID) || verdict.Deployment == "" || !nodeConfigDigest(verdict.RenderingSHA256) || !nodeConfigDigest(verdict.OperationsSHA256) {
		return verdict, fmt.Errorf("node-config verdict does not bind this invocation and exact operands")
	}
	return verdict, nil
}

func nodeConfigDigest(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32 && hex.EncodeToString(decoded) == value
}
