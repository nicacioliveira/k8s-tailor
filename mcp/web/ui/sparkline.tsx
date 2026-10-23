import * as React from "react";

import { cn } from "./lib/cn";

interface SparklineProps extends Omit<React.SVGAttributes<SVGSVGElement>, "fill"> {
  data: number[];
  width?: number;
  height?: number;
  strokeWidth?: number;
  fill?: boolean;
}

function Sparkline({
  data,
  width = 80,
  height = 24,
  strokeWidth = 1.5,
  fill = true,
  className,
  ...rest
}: SparklineProps) {
  const gradientId = React.useId();
  if (data.length < 2) return null;

  const min = Math.min(...data);
  const max = Math.max(...data);
  const range = max - min || 1;
  const stepX = width / (data.length - 1);
  const y = (v: number) => height - ((v - min) / range) * height;
  const points = data.map((v, i) => `${(i * stepX).toFixed(2)},${y(v).toFixed(2)}`);
  const last = data[data.length - 1] ?? 0;

  return (
    <svg
      width={width}
      height={height}
      viewBox={`0 0 ${width} ${height}`}
      preserveAspectRatio="none"
      aria-hidden
      className={cn("overflow-visible", className)}
      {...rest}
    >
      {fill && (
        <>
          <defs>
            <linearGradient id={gradientId} x1="0" y1="0" x2="0" y2="1">
              <stop offset="0%" stopColor="currentColor" stopOpacity="0.25" />
              <stop offset="100%" stopColor="currentColor" stopOpacity="0" />
            </linearGradient>
          </defs>
          <path
            d={`M0,${height} L${points.join(" L")} L${width},${height} Z`}
            fill={`url(#${gradientId})`}
          />
        </>
      )}
      <polyline
        fill="none"
        stroke="currentColor"
        strokeWidth={strokeWidth}
        strokeLinecap="round"
        strokeLinejoin="round"
        points={points.join(" ")}
      />
      <circle cx={width} cy={y(last)} r={strokeWidth + 0.5} fill="currentColor" />
    </svg>
  );
}

export { Sparkline };
export type { SparklineProps };
