export default {
  customModels: {
    title: 'Custom Models',
    description: 'Manage custom model definitions with system prompt injection',
    createCustomModel: 'Create Custom Model',
    editCustomModel: 'Edit Custom Model',
    deleteCustomModel: 'Delete Custom Model',
    searchPlaceholder: 'Search custom models...',
    noData: 'No custom models found',
    notExposed: 'Not exposed',
    unknownGroup: 'Group #{id}',
    confirmDelete: "Are you sure you want to delete custom model '{modelId}'?",
    
    // Table columns
    columns: {
      modelId: 'Model ID',
      upstreamGroup: 'Upstream Group',
      upstreamModel: 'Upstream Model',
      downstreamGroups: 'Downstream Groups',
      enabled: 'Enabled',
      description: 'Description',
      createdAt: 'Created',
      actions: 'Actions'
    },
    
    // Form fields
    form: {
      modelId: 'Model ID',
      modelIdPlaceholder: 'e.g., my-team/gpt-4.1',
      modelIdHelp: 'Unique model ID used by clients. Dots and slashes are supported. Cannot be changed after creation.',
      upstreamGroup: 'Upstream Group',
      upstreamGroupPlaceholder: 'Select upstream group',
      upstreamGroupHelp: 'Requests use this group’s accounts. The group must be active to serve requests.',
      upstreamModel: 'Upstream Model',
      upstreamModelPlaceholder: 'e.g., gpt-4, claude-3-opus',
      upstreamModelHelp: 'The actual upstream model name. Only one hop is allowed: another custom model cannot be used as the upstream.',
      systemPrompt: 'System Prompt',
      systemPromptPlaceholder: 'Enter system prompt to inject...',
      systemPromptHelp: 'Optional system prompt that will be injected into all requests',
      injectionMode: 'Injection Mode',
      injectionModeHelp: 'prepend: before the client system prompt; append: after it; replace: override the client system prompt entirely.',
      injectionModePrepend: 'Prepend',
      injectionModeAppend: 'Append',
      injectionModeReplace: 'Replace',
      description: 'Description',
      descriptionPlaceholder: 'Describe this custom model...',
      downstreamGroups: 'Downstream Groups',
      groupSearch: 'Search groups by name, platform or status',
      downstreamGroupsHelp: 'Choose the existing groups that can access this model. Leave empty to save it without exposing it to any group.',
      enabled: 'Enabled',
      enabledHelp: 'Enable or disable this custom model'
    },
    
    // Validation
    validation: {
      modelIdRequired: 'Model ID is required',
      upstreamGroupRequired: 'Upstream group is required',
      upstreamModelRequired: 'Upstream model is required'
    },
    
    // Messages
    messages: {
      createSuccess: 'Custom model created successfully',
      updateSuccess: 'Custom model updated successfully',
      deleteSuccess: 'Custom model deleted successfully',
      createError: 'Failed to create custom model',
      updateError: 'Failed to update custom model',
      deleteError: 'Failed to delete custom model',
      loadError: 'Failed to load custom models',
      groupsError: 'Failed to load groups. Refresh to try again.'
    },
    
    // Actions
    actions: {
      create: 'Create',
      edit: 'Edit',
      delete: 'Delete',
      enable: 'Enable',
      disable: 'Disable',
      cancel: 'Cancel',
      save: 'Save',
      clearSelection: 'Clear selection'
    },
    
    // Status
    status: {
      enabled: 'Enabled',
      disabled: 'Disabled',
      active: 'Active',
      inactive: 'Inactive'
    },
    
    // Filters
    filters: {
      allStatus: 'All Status',
      enabledOnly: 'Enabled Only',
      disabledOnly: 'Disabled Only'
    }
  }
}
