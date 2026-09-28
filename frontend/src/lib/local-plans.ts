export function localPlanLabel(plan: string) {
  return ({plus:'ChatGPT Plus',go:'ChatGPT Go',pro_5x:'GPTPRO5x卡冲升级',pro_5x_cl:'ChatGPT Pro 5X 智利区',pro_20x:'ChatGPT Pro 20X',credit250:'Codex 点数 250',credit500:'Codex 点数 500',credit1000:'Codex 点数 1000',credit2500:'Codex 点数 2500',credit5000:'Codex 点数 5000',credit25000:'Codex 点数 25000'} as Record<string,string>)[plan] || plan
}
