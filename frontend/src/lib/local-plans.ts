export function localPlanLabel(plan: string) {
  return ({plus:'ChatGPT Plus',go:'ChatGPT Go',pro_5x:'GPTPRO5x卡冲升级',pro_20x:'ChatGPT Pro 20X'} as Record<string,string>)[plan] || plan
}
