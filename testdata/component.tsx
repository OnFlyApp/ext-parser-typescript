import React from "react";
/** Greets. */
export default function Hello({ name }: { name: string }) {
  return <div>{name}</div>;
}
export const Arrow = <T,>(p: T) => <span>{String(p)}</span>;
