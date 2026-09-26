/** Editorial context for the synthetic, public presentation projects. */
export type DemoStory = {
  eyebrow: string;
  headline: string;
  question: string;
  interpretation: string;
  caveat: string;
  methods: string[];
  observations: string[];
  nextStep: string;
};

export const demoStories: Record<string, DemoStory> = {
  "demo-mof-humidity-separation": {
    eyebrow: "MOF-X · humidity-dependent separation · 298 K",
    headline: "Does MOF-X retain C₂H₄/C₂H₆ selectivity at 40–70% relative humidity?",
    question: "Does MOF-X retain C₂H₄/C₂H₆ separation selectivity at 298 K and 40–70% relative humidity?",
    interpretation: "The synthetic campaign supports a dry-gas baseline and a smaller change at 40% RH. At 70% RH, selectivity falls substantially; competitive water adsorption and framework change remain competing explanations.",
    caveat: "All measurements, calculations, people, and institutions in this presentation are synthetic demonstration records. They are not experimental claims.",
    methods: ["Three replicate adsorption isotherms at each humidity condition", "IAST selectivity calculation with recorded inputs", "Activation protocol variants, PXRD interpretation, and a DFT comparison"],
    observations: ["Dry-gas IAST selectivity: 12.4 ± 0.6 (synthetic)", "40% RH: at least 90% of the dry-gas selectivity in the synthetic campaign", "70% RH: approximately 60% lower selectivity than the dry baseline"],
    nextStep: "Repeat the 70% RH experiment with independent sample preparation and compare activation history before choosing a dominant mechanism.",
  },
  "demo-mofx-independent-replication": {
    eyebrow: "Independent replication · 70% RH · 298 K",
    headline: "Can a second laboratory reproduce the humidity result?",
    question: "Does the 70% RH selectivity loss reproduce with an independently prepared MOF-X sample?",
    interpretation: "The synthetic independent run shows a smaller loss than the source campaign. This disagreement is preserved as a separate contribution, rather than silently averaged into the original conclusion.",
    caveat: "Synthetic example only. The displayed CSV and results are demonstration fixtures, not laboratory measurements.",
    methods: ["Separately prepared MOF-X sample", "Documented 180 °C vacuum activation and 70% RH protocol", "Three synthetic replicate isotherms and an explicit comparison with the source campaign"],
    observations: ["The independent result does not reproduce the reported magnitude of selectivity loss.", "Activation history is a plausible source of the disagreement.", "A contested finding records the uncertainty without overwriting the source project."],
    nextStep: "Exchange samples and repeat activation under one shared protocol, then compare the three replicate distributions.",
  },
  "demo-mofy-transferability-study": {
    eyebrow: "Computational transferability · MOF-Y · 298 K",
    headline: "Does the proposed mechanism transfer to an analogue?",
    question: "Does the water-competition interpretation transfer from MOF-X to an isostructural MOF-Y analogue?",
    interpretation: "The synthetic model predicts lower water occupancy in MOF-Y and a weaker humidity effect. This is a model-based comparison, not experimental validation.",
    caveat: "MOF-Y, calculations, and outputs are synthetic demonstration fixtures. No real DFT job was run.",
    methods: ["Structured MOF-Y material record", "PBE-D3-labelled comparison at 70% RH", "Recorded model inputs and a CSV output for reproducible inspection"],
    observations: ["Predicted water occupancy is lower in MOF-Y than in the source MOF-X model.", "The model suggests partial transferability of the water-competition mechanism.", "Experimental confirmation remains open."],
    nextStep: "Prepare MOF-Y independently and measure adsorption under the same humidity and activation conditions.",
  },
};
