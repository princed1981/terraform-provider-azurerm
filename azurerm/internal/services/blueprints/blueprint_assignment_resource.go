package blueprints

import (
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/terraform-providers/terraform-provider-azurerm/azurerm/helpers/set"

	"github.com/Azure/azure-sdk-for-go/services/preview/blueprint/mgmt/2018-11-01-preview/blueprint"
	"github.com/hashicorp/terraform-plugin-sdk/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/helper/structure"
	"github.com/hashicorp/terraform-plugin-sdk/helper/validation"
	"github.com/terraform-providers/terraform-provider-azurerm/azurerm/helpers/azure"
	"github.com/terraform-providers/terraform-provider-azurerm/azurerm/helpers/suppress"
	"github.com/terraform-providers/terraform-provider-azurerm/azurerm/internal/clients"
	"github.com/terraform-providers/terraform-provider-azurerm/azurerm/internal/location"
	"github.com/terraform-providers/terraform-provider-azurerm/azurerm/internal/services/blueprints/parse"
	"github.com/terraform-providers/terraform-provider-azurerm/azurerm/internal/services/blueprints/validate"
	"github.com/terraform-providers/terraform-provider-azurerm/azurerm/internal/timeouts"
	"github.com/terraform-providers/terraform-provider-azurerm/azurerm/utils"
)

func resourceArmBlueprintAssignment() *schema.Resource {
	return &schema.Resource{
		Create: resourceArmBlueprintAssignmentCreateUpdate,
		Update: resourceArmBlueprintAssignmentCreateUpdate,
		Read:   resourceArmBlueprintAssignmentRead,
		Delete: resourceArmBlueprintAssignmentDelete,

		Importer: nil,

		Timeouts: &schema.ResourceTimeout{
			Read: schema.DefaultTimeout(5 * time.Minute),
		},

		Schema: map[string]*schema.Schema{
			"name": {
				Type:         schema.TypeString,
				Required:     true,
				ValidateFunc: validation.StringIsNotEmpty,
			},

			"scope_type": {
				Type:     schema.TypeString,
				Required: true,
				ValidateFunc: validation.StringInSlice([]string{
					"subscription",
					"managementGroup",
				}, true),
			},

			"scope": {
				Type:         schema.TypeString,
				Required:     true,
				ValidateFunc: validation.StringIsNotEmpty,
			},

			"location": location.Schema(),

			"identity": ManagedIdentitySchema(),

			"blueprint_id": {
				Type:         schema.TypeString,
				Optional:     true,
				Computed:     true,
				ValidateFunc: validate.BlueprintID,
			},

			"version_name": {
				Type:         schema.TypeString,
				Optional:     true,
				Computed:     true,
				ValidateFunc: validation.StringIsNotEmpty,
			},

			"version_id": {
				Type:         schema.TypeString,
				Optional:     true,
				Computed:     true,
				ValidateFunc: validate.BlueprintVersionID,
			},

			"parameter_values": {
				Type:             schema.TypeString,
				Optional:         true,
				StateFunc:        normalizeAssignmentParameterValuesJSON,
				ValidateFunc:     validation.StringIsJSON,
				DiffSuppressFunc: structure.SuppressJsonDiff,
			},

			"resource_groups": {
				Type:             schema.TypeString,
				Optional:         true,
				StateFunc:        normalizeAssignmentResourceGroupValuesJSON,
				ValidateFunc:     validation.StringIsJSON,
				DiffSuppressFunc: structure.SuppressJsonDiff,
			},

			"lock_mode": {
				Type:     schema.TypeString,
				Optional: true,
				Default:  string(blueprint.None),
				ValidateFunc: validation.StringInSlice([]string{
					string(blueprint.None),
					string(blueprint.AllResourcesReadOnly),
					string(blueprint.AllResourcesDoNotDelete),
				}, false),
				// The first character of value returned by the service is always in lower case.
				DiffSuppressFunc: suppress.CaseDifference,
			},

			"lock_exclude_principals": {
				Type:     schema.TypeSet,
				Optional: true,
				MaxItems: 5,
				Elem: &schema.Schema{
					Type:         schema.TypeString,
					ValidateFunc: validation.IsUUID,
				},
				Set: schema.HashString,
			},

			"description": {
				Type:     schema.TypeString,
				Computed: true,
			},

			"display_name": {
				Type:     schema.TypeString,
				Computed: true,
			},

			"blueprint_name": {
				Type:     schema.TypeString,
				Computed: true,
			},

			"type": {
				Type:     schema.TypeString,
				Computed: true,
			},
		},
	}
}

func resourceArmBlueprintAssignmentCreateUpdate(d *schema.ResourceData, meta interface{}) error {
	client := meta.(*clients.Client).Blueprints.AssignmentsClient
	ctx, cancel := timeouts.ForCreateUpdate(meta.(*clients.Client).StopContext, d)
	defer cancel()

	var name, targetScope, definitionScope, blueprintId string

	if versionIdRaw, ok := d.GetOk("version_id"); ok {
		blueprintId = versionIdRaw.(string)
		if _, ok := d.GetOk("blueprint_id"); ok {
			return fmt.Errorf("cannot specify `blueprint_id` when `version_id` is specified")
		}

		if _, ok := d.GetOk("version_name"); ok {
			return fmt.Errorf("cannot specify `version_name` when `version_id` is specified")
		}
	} else {
		if bpIDRaw, ok := d.GetOk("blueprint_id"); ok {
			bpID, err := parse.DefinitionID(bpIDRaw.(string))
			if err != nil {
				return err
			}

			if versionName, ok := d.GetOk("version_name"); ok {
				blueprintId = fmt.Sprintf("%s/versions/%s", bpIDRaw.(string), versionName.(string))
				definitionScope = bpID.Scope
			} else {
				return fmt.Errorf("`version_name` must be specified if `version_id` is not supplied")
			}
		} else {
			return fmt.Errorf("`blueprint_id` must be specified if `version_id` is not supplied")
		}
	}

	targetScope = fmt.Sprintf("%s/%s", d.Get("scope_type"), d.Get("Scope"))
	name = d.Get("name").(string)

	assignment := blueprint.Assignment{
		Identity: nil, // TODO - Identity schema?
		AssignmentProperties: &blueprint.AssignmentProperties{
			BlueprintID: utils.String(blueprintId), // This is mislabeled - The ID is that of the Published Version, not just the Blueprint
			Scope:       utils.String(definitionScope),
		},
		Location: utils.String(azure.NormalizeLocation(d.Get("location"))),
	}

	if lockModeRaw, ok := d.GetOk("lock_mode"); ok {
		assignmentLockSettings := &blueprint.AssignmentLockSettings{}
		lockMode := lockModeRaw.(string)
		assignmentLockSettings.Mode = blueprint.AssignmentLockMode(lockMode)
		if lockMode != "None" {
			excludedPrincipalsRaw := d.Get("lock_exclude_principals").(*schema.Set).List()
			if len(excludedPrincipalsRaw) != 0 {
				assignmentLockSettings.ExcludedPrincipals = utils.ExpandStringSlice(excludedPrincipalsRaw)
			}
		}
		assignment.AssignmentProperties.Locks = assignmentLockSettings
	}

	identity, err := expandArmBlueprintAssignmentIdentity(d.Get("identity").([]interface{}))
	if err != nil {
		return err
	}
	assignment.Identity = identity

	if paramValuesRaw, ok := d.GetOk("parameter_values"); ok {
		assignment.Parameters = expandArmBlueprintAssignmentParameters(paramValuesRaw.(string))
	}

	if resourceGroupsRaw, ok := d.GetOk("resource_groups"); ok {
		assignment.ResourceGroups = expandArmBlueprintAssignmentResourceGroups(resourceGroupsRaw.(string))
	}

	resp, err := client.CreateOrUpdate(ctx, targetScope, name, assignment)
	if err != nil {
		return err
	}

	stateConf := &resource.StateChangeConf{
		Pending: []string{
			string(blueprint.Waiting),
			string(blueprint.Validating),
			string(blueprint.Creating),
			string(blueprint.Deploying),
			string(blueprint.Locking),
		},
		Target:  []string{string(blueprint.Succeeded)},
		Refresh: blueprintAssignmentCreateStateRefreshFunc(ctx, client, targetScope, name),
		Timeout: d.Timeout(schema.TimeoutCreate),
	}

	if _, err := stateConf.WaitForState(); err != nil {
		return fmt.Errorf("Failed waiting for Blueprint Assignment %q (Scope %q): %+v", name, targetScope, err)
	}

	d.SetId(*resp.ID)

	return resourceArmBlueprintAssignmentRead(d, meta)
}

func resourceArmBlueprintAssignmentRead(d *schema.ResourceData, meta interface{}) error {
	client := meta.(*clients.Client).Blueprints.AssignmentsClient
	ctx, cancel := timeouts.ForRead(meta.(*clients.Client).StopContext, d)
	defer cancel()

	id, err := parse.AssignmentID(d.Id())
	if err != nil {
		return err
	}

	resourceScope := id.Scope
	assignmentName := id.Name

	resp, err := client.Get(ctx, resourceScope, assignmentName)
	if err != nil {
		if utils.ResponseWasNotFound(resp.Response) {
			log.Printf("[INFO] the Blueprint Assignment %q does not exist - removing from state", assignmentName)
			d.SetId("")
			return nil
		}

		return fmt.Errorf("Read failed for Blueprint Assignment (%q): %+v", assignmentName, err)
	}

	if resp.Name != nil {
		d.Set("name", resp.Name)
	}

	if resp.Scope != nil {
		scopeParts := strings.Split(*resp.Scope, "/")
		if len(scopeParts) == 2 {
			d.Set("scope_type", scopeParts[0])
			d.Set("scope", scopeParts[1])
		} else {
			return fmt.Errorf("read on Assignment scope failed, got: %+v", resp.Scope)
		}
	}

	if resp.Location != nil {
		d.Set("location", azure.NormalizeLocation(resp.Location))
	}

	if resp.Identity != nil {
		d.Set("identity", flattenArmBlueprintAssignmentIdentity(resp.Identity))
	}

	if resp.AssignmentProperties != nil {
		if resp.AssignmentProperties.BlueprintID != nil {
			d.Set("version_id", resp.AssignmentProperties.BlueprintID)
			bpID, versionName := splitPublishedVersionID(*resp.BlueprintID)
			d.Set("blueprint_id", bpID)
			d.Set("version_name", versionName)
		}

		if resp.Parameters != nil {
			params, err := flattenArmBlueprintAssignmentParameters(resp.Parameters)
			if err != nil {
				return err
			}
			d.Set("parameter_values", params)
		}

		if resp.ResourceGroups != nil {
			resourceGroups, err := flattenArmBlueprintAssignmentResourceGroups(resp.ResourceGroups)
			if err != nil {
				return err
			}
			d.Set("resource_groups", resourceGroups)
		}

		// Locks
		if locks := resp.Locks; locks != nil {
			d.Set("lock_mode", locks.Mode)
			if locks.ExcludedPrincipals != nil {
				d.Set("lock_excluded_principals", set.FromStringSlice(*locks.ExcludedPrincipals))
			}
		}
	}

	if resp.DisplayName != nil {
		d.Set("display_name", resp.DisplayName)
	}

	if resp.Description != nil {
		d.Set("description", resp.Description)
	}

	return nil
}

func resourceArmBlueprintAssignmentDelete(d *schema.ResourceData, meta interface{}) error {
	client := meta.(*clients.Client).Blueprints.AssignmentsClient
	ctx, cancel := timeouts.ForDelete(meta.(*clients.Client).StopContext, d)
	defer cancel()

	assignmentID, err := parse.AssignmentID(d.Id())
	if err != nil {
		return err
	}

	name := assignmentID.Name
	targetScope := assignmentID.Scope

	resp, err := client.Delete(ctx, targetScope, name)
	if err != nil {
		if utils.ResponseWasNotFound(resp.Response) {
			return nil
		}
		return fmt.Errorf("failed to delete Blueprint Assignment %q from scope %q: %+v", name, targetScope, err)
	}

	stateConf := &resource.StateChangeConf{
		Pending: []string{
			string(blueprint.Waiting),
			string(blueprint.Validating),
			string(blueprint.Locking),
			string(blueprint.Deleting),
		},
		Target:  []string{"NotFound"},
		Refresh: blueprintAssignmentDeleteStateRefreshFunc(ctx, client, targetScope, name),
		Timeout: d.Timeout(schema.TimeoutDelete),
	}

	if _, err := stateConf.WaitForState(); err != nil {
		return fmt.Errorf("Failed waiting for Blueprint Assignment %q (Scope %q): %+v", name, targetScope, err)
	}

	return nil
}
