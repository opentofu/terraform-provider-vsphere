// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: MPL-2.0

package vmworkflow

import (
	"fmt"
	"log"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/validation"
	"github.com/hashicorp/terraform-provider-vsphere/vsphere/internal/helper/datastore"
	"github.com/hashicorp/terraform-provider-vsphere/vsphere/internal/helper/guestoscustomizations"
	"github.com/hashicorp/terraform-provider-vsphere/vsphere/internal/helper/hostsystem"
	"github.com/hashicorp/terraform-provider-vsphere/vsphere/internal/helper/resourcepool"
	"github.com/hashicorp/terraform-provider-vsphere/vsphere/internal/helper/virtualmachine"
	"github.com/hashicorp/terraform-provider-vsphere/vsphere/internal/virtualdevice"
	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

// VirtualMachineCloneSchema represents the schema for the VM clone sub-resource.
//
// This is a workflow for vsphere_virtual_machine that facilitates the creation
// of a virtual machine through cloning from an existing template.
// Customization is nested here, even though it exists in its own workflow.
func VirtualMachineCloneSchema() map[string]*schema.Schema {
	customizatonSpecSchema := guestoscustomizations.SpecSchema(true)
	customizatonSpecSchema["timeout"] = &schema.Schema{
		Type:        schema.TypeInt,
		Optional:    true,
		Default:     10,
		Description: "The amount of time, in minutes, to wait for guest OS customization to complete before returning with an error. Setting this value to 0 or a negative value skips the waiter. Default: 10.",
	}

	return map[string]*schema.Schema{
		"template_uuid": {
			Type:        schema.TypeString,
			Required:    true,
			Description: "The UUID of the source virtual machine or template.",
		},
		"linked_clone": {
			Type:        schema.TypeBool,
			Optional:    true,
			Description: "Whether or not to create a linked clone when cloning. When this option is used, the source VM must have a single snapshot associated with it.",
		},
		"timeout": {
			Type:         schema.TypeInt,
			Optional:     true,
			Default:      30,
			Description:  "The timeout, in minutes, to wait for the virtual machine clone to complete.",
			ValidateFunc: validation.IntAtLeast(10),
		},
		"customize": {
			Type:          schema.TypeList,
			Optional:      true,
			MaxItems:      1,
			ConflictsWith: []string{"clone.0.customization_spec"},
			Description:   "The customization specification for the virtual machine post-clone.",
			Elem:          &schema.Resource{Schema: customizatonSpecSchema},
		},
		"customization_spec": {
			Type:          schema.TypeList,
			Optional:      true,
			MaxItems:      1,
			Description:   "The customization specification for the virtual machine post-clone.",
			ConflictsWith: []string{"clone.0.customize"},
			Elem: &schema.Resource{
				Schema: map[string]*schema.Schema{
					"id": {
						Type:        schema.TypeString,
						Required:    true,
						Description: "The unique identifier of the customization specification is its name and is unique per vCenter Server instance.",
					},
					"timeout": {
						Type:        schema.TypeInt,
						Optional:    true,
						Default:     10,
						Description: "The amount of time, in minutes, to wait for guest OS customization to complete before returning with an error. Setting this value to 0 or a negative value skips the waiter. Default: 10.",
					},
				},
			},
		},
		"ovf_network_map": {
			Type:        schema.TypeMap,
			Optional:    true,
			Description: "Mapping of ovf networks to the networks to use in vSphere.",
			Elem:        &schema.Schema{Type: schema.TypeString},
		},
		"ovf_storage_map": {
			Type:        schema.TypeMap,
			Optional:    true,
			Description: "Mapping of ovf storage to the datastores to use in vSphere.",
			Elem:        &schema.Schema{Type: schema.TypeString},
		},
	}
}

// ValidateVirtualMachineClone does pre-creation validation of a virtual
// machine's configuration to make sure it's suitable for use in cloning.
// This includes, but is not limited to checking to make sure that the disks in
// the new VM configuration line up with the configuration in the existing
// template, and checking to make sure that the VM has a single snapshot we can
// use in the even that linked clones are enabled.
func ValidateVirtualMachineClone(d *schema.ResourceDiff, c *govmomi.Client) error {
	tUUID := d.Get("clone.0.template_uuid").(string)
	if d.NewValueKnown("clone.0.template_uuid") {
		log.Printf("[DEBUG] ValidateVirtualMachineClone: Validating fitness of source VM/template %s", tUUID)
		vm, err := virtualmachine.FromUUID(c, tUUID)
		if err != nil {
			return fmt.Errorf("cannot locate virtual machine or template with UUID %q: %s", tUUID, err)
		}
		vprops, err := virtualmachine.Properties(vm)
		if err != nil {
			return fmt.Errorf("error fetching virtual machine or template properties: %s", err)
		}
		// Check to see if our guest IDs match.
		eGuestID := vprops.Config.GuestId
		aGuestID := d.Get("guest_id").(string)
		if eGuestID != aGuestID {
			return fmt.Errorf("invalid guest ID %q for clone. Please set it to %q", aGuestID, eGuestID)
		}
		// If linked clone is enabled, check to see if we have a snapshot. There need
		// to be a single snapshot on the template for it to be eligible.
		linked := d.Get("clone.0.linked_clone").(bool)
		if linked {
			if vprops.Config.Template {
				log.Printf("[DEBUG] ValidateVirtualMachineClone: Virtual machine %s is marked as a template and satisfies linked clone eligibility", tUUID)
			} else {
				log.Printf("[DEBUG] ValidateVirtualMachineClone: Checking snapshots on %s for linked clone eligibility", tUUID)
				if err := validateCloneSnapshots(vprops); err != nil {
					return err
				}
			}
		}
		// Check to make sure the disks for this VM/template line up with the disks
		// in the configuration. This is in the virtual device package, so pass off
		// to that now.
		l := object.VirtualDeviceList(vprops.Config.Hardware.Device)
		if err := virtualdevice.DiskCloneValidateOperation(d, c, l, linked); err != nil {
			return err
		}
		vconfig := vprops.Config.VAppConfig
		if vconfig != nil {
			// We need to set the vApp transport types here so that it is available
			// later in CustomizeDiff where transport requirements are validated in
			// ValidateVAppTransport
			_ = d.SetNew("vapp_transport", vconfig.GetVmConfigInfo().OvfEnvironmentTransport)
		}
	} else {
		log.Printf("[DEBUG] ValidateVirtualMachineClone: template_uuid is not available. Skipping template validation.")
	}

	// If a customization spec was defined, we need to check some items in it as well.
	if len(d.Get("clone.0.customize").([]interface{})) > 0 {
		if poolID, ok := d.GetOk("resource_pool_id"); ok {
			pool, err := resourcepool.FromID(c, poolID.(string))
			if err != nil {
				return fmt.Errorf("could not find resource pool ID %q: %s", poolID, err)
			}

			// Retrieving the vm/template data to extract the hardware version.
			// If there's a higher hardware version specified in the spec that value is used instead.
			vm, err := virtualmachine.FromUUID(c, tUUID)
			if err != nil {
				return fmt.Errorf("cannot locate virtual machine or template with UUID %q: %s", tUUID, err)
			}
			vprops, err := virtualmachine.Properties(vm)
			if err != nil {
				return fmt.Errorf("error fetching virtual machine or template properties: %s", err)
			}
			vmHardwareVersion := virtualmachine.GetHardwareVersionNumber(vprops.Config.Version)
			vmSpecHardwareVersion := d.Get("hardware_version").(int)
			if vmSpecHardwareVersion > vmHardwareVersion {
				vmHardwareVersion = vmSpecHardwareVersion
			}

			// Retrieving the guest OS family of the vm/template.
			family, err := resourcepool.OSFamily(c, pool, d.Get("guest_id").(string), vmHardwareVersion)
			if err != nil {
				return fmt.Errorf("cannot find OS family for guest ID %q: %s", d.Get("guest_id").(string), err)
			}
			// Validating the customization spec is valid for the vm/template's guest OS family
			if err := guestoscustomizations.ValidateCustomizationSpec(d, family, true); err != nil {
				return err
			}
		} else {
			log.Printf("[DEBUG] ValidateVirtualMachineClone: resource_pool_id is not available. Skipping OS family check.")
		}
	}
	log.Printf("[DEBUG] ValidateVirtualMachineClone: Source VM/template %s is a suitable source for cloning", tUUID)
	return nil
}

// validateCloneSnapshots checks a VM to make sure it has a single snapshot
// with no children, to make sure there is no ambiguity when selecting a
// snapshot for linked clones.
func validateCloneSnapshots(props *mo.VirtualMachine) error { // Ensure that the virtual machine has a snapshot attribute that we can check
	if props.Snapshot == nil {
		return fmt.Errorf("virtual machine %s must have a snapshot to be used as a linked clone", props.Config.Uuid)
	}

	// Root snapshot list can only have a singular element
	if len(props.Snapshot.RootSnapshotList) != 1 {
		return fmt.Errorf("virtual machine %s must have exactly one root snapshot (has: %d)", props.Config.Uuid, len(props.Snapshot.RootSnapshotList))
	}
	// Check to make sure the root snapshot has no children
	if len(props.Snapshot.RootSnapshotList[0].ChildSnapshotList) > 0 {
		return fmt.Errorf("virtual machine %s's root snapshot must not have children", props.Config.Uuid)
	}
	// Current snapshot must match root snapshot (this should be the case anyway)
	if props.Snapshot.CurrentSnapshot.Value != props.Snapshot.RootSnapshotList[0].Snapshot.Value {
		return fmt.Errorf("virtual machine %s's current snapshot must match root snapshot", props.Config.Uuid)
	}
	return nil
}

// ExpandVirtualMachineCloneSpec creates a clone spec for an existing virtual machine.
//
// The clone spec built by this function for the clone contains the target
// datastore, the source snapshot in the event of linked clones, and a relocate
// spec that contains the new locations and configuration details of the new
// virtual disks.
func ExpandVirtualMachineCloneSpec(d *schema.ResourceData, c *govmomi.Client) (types.VirtualMachineCloneSpec, *object.VirtualMachine, error) {
	var spec types.VirtualMachineCloneSpec
	log.Printf("[DEBUG] ExpandVirtualMachineCloneSpec: Preparing clone spec for VM")

	// Populate the datastore only if we have a datastore ID. The ID may not be
	// specified in the event a datastore cluster is specified instead.
	if dsID, ok := d.GetOk("datastore_id"); ok {
		ds, err := datastore.FromID(c, dsID.(string))
		if err != nil {
			return spec, nil, fmt.Errorf("error locating datastore for VM: %s", err)
		}
		spec.Location.Datastore = types.NewReference(ds.Reference())
	}

	tUUID := d.Get("clone.0.template_uuid").(string)
	log.Printf("[DEBUG] ExpandVirtualMachineCloneSpec: Cloning from UUID: %s", tUUID)
	vm, err := virtualmachine.FromUUID(c, tUUID)
	if err != nil {
		return spec, nil, fmt.Errorf("cannot locate virtual machine or template with UUID %q: %s", tUUID, err)
	}
	vprops, err := virtualmachine.Properties(vm)
	if err != nil {
		return spec, nil, fmt.Errorf("error fetching virtual machine or template properties: %s", err)
	}
	// If we are creating a linked clone, grab the current snapshot of the
	// source, and populate the appropriate field. This should have already been
	// validated, but just in case, validate it again here.
	if d.Get("clone.0.linked_clone").(bool) {
		log.Printf("[DEBUG] ExpandVirtualMachineCloneSpec: Clone type is a linked clone")
		log.Printf("[DEBUG] ExpandVirtualMachineCloneSpec: Fetching snapshot for VM/template UUID %s", tUUID)

		// If our properties tell us that the Template flag is set, then we need to use a
		// different option to clone the disk so that way vSphere knows the disk is shared.
		if vprops.Config.Template {
			log.Printf("[DEBUG] Virtual machine %s was marked as a template", tUUID)
			spec.Location.DiskMoveType = string(types.VirtualMachineRelocateDiskMoveOptionsMoveAllDiskBackingsAndAllowSharing)
		} else {
			// Otherwise this is a virtual machine, and in order to use our default option
			// we'll need to ensure that there's a snapshot that we can clone the disk from.
			log.Printf("[DEBUG] Virtual machine %s is a regular virtual machine", tUUID)
			if err := validateCloneSnapshots(vprops); err != nil {
				return spec, nil, err
			}
			spec.Snapshot = vprops.Snapshot.CurrentSnapshot
			log.Printf("[DEBUG] ExpandVirtualMachineCloneSpec: Using current snapshot for clone: %s", vprops.Snapshot.CurrentSnapshot.Value)

			spec.Location.DiskMoveType = string(types.VirtualMachineRelocateDiskMoveOptionsCreateNewChildDiskBacking)
		}
		log.Printf("[DEBUG] ExpandVirtualMachineCloneSpec: Using the disk move type as \"%s\"", spec.Location.DiskMoveType)
	}

	// Set the target host system and resource pool.
	poolID := d.Get("resource_pool_id").(string)
	pool, err := resourcepool.FromID(c, poolID)
	if err != nil {
		return spec, nil, fmt.Errorf("could not find resource pool ID %q: %s", poolID, err)
	}
	var hs *object.HostSystem
	if v, ok := d.GetOk("host_system_id"); ok {
		hsID := v.(string)
		var err error
		if hs, err = hostsystem.FromID(c, hsID); err != nil {
			return spec, nil, fmt.Errorf("error locating host system at ID %q: %s", hsID, err)
		}
	}
	// Validate that the host is part of the resource pool before proceeding
	if err := resourcepool.ValidateHost(c, pool, hs); err != nil {
		return spec, nil, err
	}
	poolRef := pool.Reference()
	spec.Location.Pool = &poolRef
	if hs != nil {
		hsRef := hs.Reference()
		spec.Location.Host = &hsRef
	}

	// Grab the relocate spec for the disks.
	l := object.VirtualDeviceList(vprops.Config.Hardware.Device)
	relocators, err := virtualdevice.DiskCloneRelocateOperation(d, c, l)
	if err != nil {
		return spec, nil, err
	}
	spec.Location.Disk = relocators
	log.Printf("[DEBUG] ExpandVirtualMachineCloneSpec: Clone spec prep complete")
	return spec, vm, nil
}
