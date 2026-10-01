export default {
  customModels: {
    title: '自定义模型',
    description: '管理自定义模型定义，支持系统提示词注入',
    createCustomModel: '创建自定义模型',
    editCustomModel: '编辑自定义模型',
    deleteCustomModel: '删除自定义模型',
    searchPlaceholder: '搜索自定义模型...',
    noData: '未找到自定义模型',
    notExposed: '尚未开放',
    unknownGroup: '分组 #{id}',
    confirmDelete: "确定要删除自定义模型 '{modelId}' 吗？",
    
    // 表格列
    columns: {
      modelId: '模型 ID',
      upstreamGroup: '上游分组',
      upstreamModel: '上游模型',
      downstreamGroups: '下游分组',
      enabled: '启用状态',
      description: '描述',
      createdAt: '创建时间',
      actions: '操作'
    },
    
    // 表单字段
    form: {
      modelId: '模型 ID',
      modelIdPlaceholder: '例如：my-team/gpt-4.1',
      modelIdHelp: '客户端使用的唯一模型 ID，支持点号和斜杠，创建后不可修改。',
      upstreamGroup: '上游分组',
      upstreamGroupPlaceholder: '选择上游分组',
      upstreamGroupHelp: '使用此分组的账号处理请求。分组必须处于活跃状态才能提供服务。',
      upstreamModel: '上游模型',
      upstreamModelPlaceholder: '例如：gpt-4, claude-3-opus',
      upstreamModelHelp: '实际请求的上游模型名称。仅支持单跳，不能将其他自定义模型作为上游。',
      systemPrompt: '系统提示词',
      systemPromptPlaceholder: '输入要注入的系统提示词...',
      systemPromptHelp: '可选的系统提示词，将注入到所有请求中',
      injectionMode: '注入模式',
      injectionModeHelp: 'prepend：插在客户端系统提示词之前；append：插在之后；replace：整体替换客户端系统提示词。',
      injectionModePrepend: '前置（prepend）',
      injectionModeAppend: '后置（append）',
      injectionModeReplace: '替换（replace）',
      description: '描述',
      descriptionPlaceholder: '描述此自定义模型...',
      downstreamGroups: '下游分组',
      groupSearch: '按名称、平台或状态搜索分组',
      downstreamGroupsHelp: '选择允许访问此模型的现有分组。可以留空保存，此时不会向任何分组开放。',
      enabled: '启用',
      enabledHelp: '启用或禁用此自定义模型'
    },
    
    // 验证
    validation: {
      modelIdRequired: '模型 ID 为必填项',
      upstreamGroupRequired: '上游分组为必填项',
      upstreamModelRequired: '上游模型为必填项'
    },
    
    // 消息
    messages: {
      createSuccess: '自定义模型创建成功',
      updateSuccess: '自定义模型更新成功',
      deleteSuccess: '自定义模型删除成功',
      createError: '创建自定义模型失败',
      updateError: '更新自定义模型失败',
      deleteError: '删除自定义模型失败',
      loadError: '加载自定义模型失败',
      groupsError: '加载分组失败，请刷新重试。'
    },
    
    // 操作
    actions: {
      create: '创建',
      edit: '编辑',
      delete: '删除',
      enable: '启用',
      disable: '禁用',
      cancel: '取消',
      save: '保存',
      clearSelection: '清空选择'
    },
    
    // 状态
    status: {
      enabled: '已启用',
      disabled: '已禁用',
      active: '活跃',
      inactive: '停用'
    },
    
    // 筛选
    filters: {
      allStatus: '全部状态',
      enabledOnly: '仅启用',
      disabledOnly: '仅禁用'
    }
  }
}
