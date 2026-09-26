"use client";

import { useEffect, useState } from "react";
import Link from "next/link";
import type { AssetSummary } from "../../../../../lib/assets";

type Point = { pressure: number; mean: number; sd: number };
type Panel = { humidity: number; ethylene: Point[]; ethane: Point[] };
type Row = {
  relative_humidity_pct: number;
  replicate: number;
  pressure_kpa: number;
  c2h4_uptake_mmol_g: number;
  c2h6_uptake_mmol_g: number;
};

const FIXTURES = [
  { humidity: 0, path: "/demo-data/adsorption-dry.csv" },
  { humidity: 40, path: "/demo-data/adsorption-40rh.csv" },
  { humidity: 70, path: "/demo-data/adsorption-70rh.csv" },
] as const;

export function AdsorptionChart({ sources }: { sources: AssetSummary[] }) {
  const [panels, setPanels] = useState<Panel[] | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    Promise.all(FIXTURES.map(async (fixture) => {
      const response = await fetch(fixture.path, { cache: "force-cache" });
      if (!response.ok) throw new Error(`Synthetic measurement file unavailable (${response.status}).`);
      const text = await response.text();
      const rows = parseCsv(text);
      return {
        humidity: fixture.humidity,
        ethylene: summarize(rows, "c2h4_uptake_mmol_g"),
        ethane: summarize(rows, "c2h6_uptake_mmol_g"),
      };
    }))
      .then((result) => {
        if (!cancelled) setPanels(result);
      })
      .catch((cause: unknown) => {
        if (!cancelled) setError(cause instanceof Error ? cause.message : "The synthetic figure could not be prepared.");
      });
    return () => {
      cancelled = true;
    };
  }, []);

  const sourceAssets = sources.filter((asset) => asset.slug.includes("replicates"));
  return (
    <figure className="adsorption-figure" data-chart-source="synthetic-csv-fixtures">
      <figcaption className="adsorption-figure-caption">
        Synthetic illustration · 298 K · three replicate values per pressure point · error bars show sample SD
      </figcaption>
      {panels === null && error === null ? <p className="adsorption-figure-status">Preparing figure…</p> : null}
      {error ? <p className="adsorption-figure-status" role="status">{error}</p> : null}
      {panels ? (
        <>
          <svg
            className="adsorption-figure-svg"
            viewBox="0 0 900 315"
            role="img"
            aria-labelledby="adsorption-svg-title adsorption-svg-description"
          >
            <title id="adsorption-svg-title">Synthetic adsorption isotherms at three humidity levels</title>
            <desc id="adsorption-svg-description">
              Mean C2H4 and C2H6 uptake at 0, 40, and 70 percent relative humidity versus pressure,
              with sample standard deviation from three synthetic replicate runs.
            </desc>
            {panels.map((panel, index) => (
              <g key={panel.humidity} transform={`translate(${index * 300}, 0)`}>
                <text className="adsorption-panel-title" x="150" y="19" textAnchor="middle">{panel.humidity}% RH</text>
                {[0, 1, 2, 3, 4].map((tick) => {
                  const y = yCoord(tick);
                  return (
                    <g key={tick}>
                      <line className="adsorption-gridline" x1="49" x2="270" y1={y} y2={y} />
                      <text className="adsorption-tick" x="40" y={y + 3} textAnchor="end">{tick}</text>
                    </g>
                  );
                })}
                {[0, 50, 100].map((tick) => {
                  const x = xCoord(tick);
                  return (
                    <g key={tick}>
                      <line className="adsorption-axis-tick" x1={x} x2={x} y1="240" y2="244" />
                      <text className="adsorption-tick" x={x} y="258" textAnchor="middle">{tick}</text>
                    </g>
                  );
                })}
                <line className="adsorption-axis" x1="49" x2="270" y1="240" y2="240" />
                <line className="adsorption-axis" x1="49" x2="49" y1="52" y2="240" />
                <Series points={panel.ethylene} color="#0969da" />
                <Series points={panel.ethane} color="#bf8700" />
                {index === 0 ? (
                  <text className="adsorption-y-label" transform="translate(12 156) rotate(-90)" textAnchor="middle">
                    Uptake (mmol g⁻¹)
                  </text>
                ) : null}
                <text className="adsorption-x-label" x="160" y="283" textAnchor="middle">Pressure (kPa)</text>
              </g>
            ))}
          </svg>
          <div className="adsorption-legend" aria-label="Figure legend">
            <span><i className="adsorption-legend-swatch ethylene" /> C₂H₄ uptake</span>
            <span><i className="adsorption-legend-swatch ethane" /> C₂H₆ uptake</span>
            <span className="adsorption-errorbar-key">points: mean · whiskers: ±1 SD</span>
          </div>
        </>
      ) : null}
      <div className="adsorption-source">
        <strong>Data records</strong>
        {sourceAssets.length ? (
          <ul>
            {sourceAssets.map((asset) => (
              <li key={asset.pid}><Link href={asset.url}>{asset.title}</Link> · {asset.latest_version}</li>
            ))}
          </ul>
        ) : (
          <span>Replicate dataset assets will appear here after the demo seed has been run.</span>
        )}
      </div>
      <p className="adsorption-figure-note">
        Values are fabricated for interface demonstration. They are not experimental results and should not be used as scientific evidence.
      </p>
    </figure>
  );
}

function Series({ points, color }: { points: Point[]; color: string }) {
  const path = points.map((point) => `${xCoord(point.pressure)},${yCoord(point.mean)}`).join(" ");
  return (
    <g style={{ color }}>
      {points.map((point) => {
        const x = xCoord(point.pressure);
        const top = yCoord(Math.min(5, point.mean + point.sd));
        const bottom = yCoord(Math.max(0, point.mean - point.sd));
        return (
          <g key={point.pressure} className="adsorption-errorbar">
            <line x1={x} x2={x} y1={top} y2={bottom} />
            <line x1={x - 4} x2={x + 4} y1={top} y2={top} />
            <line x1={x - 4} x2={x + 4} y1={bottom} y2={bottom} />
          </g>
        );
      })}
      <polyline className="adsorption-series-line" points={path} />
      {points.map((point) => (
        <circle
          className="adsorption-series-point"
          key={point.pressure}
          cx={xCoord(point.pressure)}
          cy={yCoord(point.mean)}
          r="3.2"
        />
      ))}
    </g>
  );
}

function parseCsv(text: string): Row[] {
  const lines = text.trim().split(/\r?\n/);
  if (lines.length < 2) throw new Error("Synthetic measurement file contains no observations.");
  const headings = lines[0].split(",");
  return lines.slice(1).map((line) => {
    const values = line.split(",");
    const row = Object.fromEntries(headings.map((heading, index) => [heading, Number(values[index])])) as Record<string, number>;
    if (Object.values(row).some((value) => !Number.isFinite(value))) {
      throw new Error("Synthetic measurement file contains a non-numeric observation.");
    }
    return {
      relative_humidity_pct: row.relative_humidity_pct,
      replicate: row.replicate,
      pressure_kpa: row.pressure_kpa,
      c2h4_uptake_mmol_g: row.c2h4_uptake_mmol_g,
      c2h6_uptake_mmol_g: row.c2h6_uptake_mmol_g,
    };
  });
}

function summarize(rows: Row[], field: "c2h4_uptake_mmol_g" | "c2h6_uptake_mmol_g"): Point[] {
  const groups = new Map<number, number[]>();
  for (const row of rows) {
    const values = groups.get(row.pressure_kpa) ?? [];
    values.push(row[field]);
    groups.set(row.pressure_kpa, values);
  }
  return [...groups.entries()].sort(([a], [b]) => a - b).map(([pressure, values]) => {
    const mean = values.reduce((sum, value) => sum + value, 0) / values.length;
    const variance = values.length > 1
      ? values.reduce((sum, value) => sum + (value - mean) ** 2, 0) / (values.length - 1)
      : 0;
    return { pressure, mean, sd: Math.sqrt(variance) };
  });
}

function xCoord(pressure: number): number {
  return 49 + (pressure / 100) * 221;
}

function yCoord(value: number): number {
  return 240 - (value / 5) * 188;
}
