/** English labels for legacy synthetic versions pinned by evidence relations. */
export function demoEnglishText(value: string): string {
  if (!/[\u3400-\u9fff]/u.test(value)) return value;
  if (value.includes("12.4") && value.includes("0.6")) return "At 298 K under dry conditions, the synthetic MOF-X C₂H₄/C₂H₆ IAST selectivity is 12.4 ± 0.6.";
  if (value.includes("90%")) return "At 40% RH and 298 K, the synthetic campaign retains at least 90% of dry-gas selectivity.";
  if (value.includes("60%") && value.includes("selectivity")) return "At 70% RH and 298 K, synthetic MOF-X selectivity is approximately 60% lower than the dry baseline.";
  if (value.includes("22 wt%")) return "After cycling at 70% RH, synthetic MOF-X retains about 22 wt% residual water.";
  if (value.includes("Cu")) return "The 70% RH selectivity loss may be driven by water competing for open Cu sites.";
  if (value.includes("骨架降解") && value.includes("竞争吸附")) return "Water competition and framework change remain competing explanations for the 70% RH selectivity loss.";
  if (value.includes("活化温度")) return "Alternative 180 °C activation protocol; a scientific conflict with main.";
  if (value.includes("机理分支")) return "Competing mechanisms: water adsorption versus framework degradation.";
  return "Earlier synthetic wording is retained in the cited version history.";
}
