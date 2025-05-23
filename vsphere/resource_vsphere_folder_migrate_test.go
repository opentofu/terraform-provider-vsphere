// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: MPL-2.0

package vsphere

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-provider-vsphere/vsphere/internal/helper/testhelper"

	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
)

const (
	testAccResourceVSphereFolderMigrateStateFolderV0Path = "Discovered virtual machine"
)

func testAccResourceVSphereFolderMigrateStatePreCheck(t *testing.T) {
	if os.Getenv("TF_ACC") == "" {
		t.Skip("set TF_ACC to run vsphere_folder state migration tests (provider connection is required)")
	}
}

func TestAccResourceVSphereFolderMigrateState_basic(t *testing.T) {
	testAccResourceVSphereFolderMigrateStatePreCheck(t)
	testAccPreCheck(t)

	is := &terraform.InstanceState{
		ID: fmt.Sprintf("%v/%v", testhelper.CombineConfigs(testhelper.ConfigDataRootDC1(), testhelper.ConfigDataRootPortGroup1()), testAccResourceVSphereFolderMigrateStateFolderV0Path),
		Attributes: map[string]string{
			"path": testAccResourceVSphereFolderMigrateStateFolderV0Path,
		},
	}
	if dc := os.Getenv("TF_VAR_VSPHERE_DATACENTER"); dc != "" {
		is.Attributes["datacenter"] = dc
	}
	meta, err := testAccProviderMeta(t)
	if err != nil {
		t.Fatalf("bad: %s", err)
	}
	is, err = resourceVSphereFolderMigrateState(0, is, meta)
	if err != nil {
		t.Fatalf("bad: %s", err)
	}
	if !strings.HasPrefix(is.ID, "group-") {
		t.Fatalf("expected ID to start with \"group-\" got ID as %q", is.ID)
	}
}

func TestAccResourceVSphereFolderMigrateState_empty(t *testing.T) {
	var is *terraform.InstanceState
	var meta interface{}

	testAccResourceVSphereFolderMigrateStatePreCheck(t)
	testAccPreCheck(t)

	// should handle nil
	is, err := resourceVSphereFolderMigrateState(0, is, meta)

	if err != nil {
		t.Fatalf("err: %#v", err)
	}
	if is != nil {
		t.Fatalf("expected nil instancestate, got: %#v", is)
	}

	// should handle non-nil but empty
	is = &terraform.InstanceState{}
	_, err = resourceVSphereFolderMigrateState(0, is, meta)

	if err != nil {
		t.Fatalf("err: %#v", err)
	}
}
