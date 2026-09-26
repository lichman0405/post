"use client";

import { useEffect, useState } from "react";

type Series = { label: string; values: number[]; color: string };
type Figure = { x: number[]; series: Series[]; xLabel: string; yLabel: string; note: string };

export function CollaborationChart({ kind }: { kind: "replication" | "transferability" }) {
  const [figure, setFigure] = useState<Figure | null>(null);
  const [error, setError] = useState<string | null>(null);
  useEffect(() => {
    let cancelled = false;
    const path = kind === "replication" ? "/demo-data/replication-70rh.csv" : "/demo-data/mofy-transfer.csv";
    fetch(path).then(async response => {
      if (!response.ok) throw new Error(`Figure data unavailable (${response.status}).`);
      return response.text();
    }).then(csv => { if (!cancelled) setFigure(parseFigure(kind, csv)); })
      .catch((cause: unknown) => { if (!cancelled) setError(cause instanceof Error ? cause.message : "Figure unavailable."); });
    return () => { cancelled = true; };
  }, [kind]);
  if (error) return <p role="status">{error}</p>;
  if (!figure) return <p>Preparing the synthetic figure…</p>;
  const maxY = kind === "replication" ? 2.5 : 1;
  const xMin = figure.x[0] ?? 0;
  const xMax = figure.x.at(-1) ?? 100;
  const x = (value: number) => 60 + (value - xMin) / (xMax - xMin || 1) * 620;
  const y = (value: number) => 235 - value / maxY * 190;
  return <figure className="collaboration-figure"><figcaption>{figure.note}</figcaption><svg viewBox="0 0 760 290" role="img" aria-label={`${figure.yLabel} versus ${figure.xLabel}; synthetic data`}>
    {[0, 0.25, 0.5, 0.75, 1].map(fraction => <g key={fraction}><line x1="60" x2="680" y1={y(fraction * maxY)} y2={y(fraction * maxY)} stroke="#e2e8ef" /><text x="48" y={y(fraction * maxY) + 4} textAnchor="end" fontSize="11" fill="#66788a">{(fraction * maxY).toFixed(kind === "replication" ? 1 : 2)}</text></g>)}
    {figure.x.map(value => <g key={value}><text x={x(value)} y="253" textAnchor="middle" fontSize="11" fill="#66788a">{value}</text></g>)}
    {figure.series.map(series => <g key={series.label}><polyline fill="none" stroke={series.color} strokeWidth="3" points={figure.x.map((value, index) => `${x(value)},${y(series.values[index] ?? 0)}`).join(" ")} />{figure.x.map((value, index) => <circle key={value} cx={x(value)} cy={y(series.values[index] ?? 0)} r="4" fill={series.color} />)}</g>)}
    <text x="370" y="281" textAnchor="middle" fontSize="12">{figure.xLabel}</text>
    <text transform="translate(13 145) rotate(-90)" textAnchor="middle" fontSize="12">{figure.yLabel}</text>
    {figure.series.map((series, index) => <g key={series.label} transform={`translate(${470 + index * 110},20)`}><line x1="0" x2="16" y1="0" y2="0" stroke={series.color} strokeWidth="3" /><text x="21" y="4" fontSize="11">{series.label}</text></g>)}
  </svg><p>Source: synthetic CSV attached to this project’s dataset or calculation record. The figure illustrates the demo data; it is not a scientific validation.</p></figure>;
}

function parseFigure(kind: "replication" | "transferability", csv: string): Figure {
  const [header, ...rows] = csv.trim().split(/\r?\n/).map(line => line.split(","));
  const records = rows.map(row => Object.fromEntries(header.map((key, index) => [key, Number(row[index])])));
  if (kind === "transferability") return {
    x: records.map(row => row.humidity_pct),
    series: [
      { label: "H₂O sites", values: records.map(row => row.water_site_occupancy), color: "#0969da" },
      { label: "C₂H₄ sites", values: records.map(row => row.ethylene_site_occupancy), color: "#bf8700" },
    ],
    xLabel: "Relative humidity (%)", yLabel: "Predicted site occupancy", note: "MOF-Y model output · 298 K · synthetic fixture",
  };
  const pressures = [...new Set(records.map(row => row.pressure_kpa))].sort((a, b) => a - b);
  const mean = (pressure: number, key: string) => { const values = records.filter(row => row.pressure_kpa === pressure).map(row => row[key]); return values.reduce((sum, value) => sum + value, 0) / values.length; };
  return {
    x: pressures,
    series: [
      { label: "C₂H₄", values: pressures.map(pressure => mean(pressure, "c2h4_uptake_mmol_g")), color: "#0969da" },
      { label: "C₂H₆", values: pressures.map(pressure => mean(pressure, "c2h6_uptake_mmol_g")), color: "#bf8700" },
    ],
    xLabel: "Pressure (kPa)", yLabel: "Mean uptake (mmol g⁻¹)", note: "Independent 70% RH adsorption · 298 K · mean of three synthetic replicates",
  };
}
