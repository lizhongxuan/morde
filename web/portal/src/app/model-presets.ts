export const modelConnectionPresets = [
  { id: 'gpt-6-luna', name: 'OpenAI GPT-6 Luna', baseURL: 'https://api.openai.com/v1', modelName: 'gpt-6-luna' },
  { id: 'gpt-6.1-sol', name: 'OpenAI GPT-6.1 Sol', baseURL: 'https://api.openai.com/v1', modelName: 'gpt-6.1-sol' },
  { id: 'gpt-6-astra', name: 'OpenAI GPT-6 Astra', baseURL: 'https://api.openai.com/v1', modelName: 'gpt-6-astra' },
  { id: 'deepseek-flash', name: 'DeepSeek Flash', baseURL: 'https://api.deepseek.com', modelName: 'deepseek-flash' },
  { id: 'glm-5.1', name: 'GLM-5.1', baseURL: 'https://api.z.ai/api/paas/v4', modelName: 'glm-5.1' },
] as const;
