import { a, b as c } from "x";
import D from "y";
export * from "z";
/** Doc for I. */
export interface I<T> extends J { a: number; m(x: T): void }
export type U = string | number;
export enum E { A = 1, B }
declare module "m" { export function f(): void }
namespace N.M { export const k = 1 }
@Component({ selector: "x" })
export class Foo<T> extends Bar implements I {
  @Input() private x: number = 1;
  static readonly y = 2;
  #z = 3;
  handle = (e: Event) => { this.x++; };
  constructor(private a: string) { super(); }
  /** Value. */
  async get v(): Promise<number> { return 1 }
  m<T>(a: T): T { return a }
  static async *it() {}
  [key: string]: any;
  static { init(); }
}
export const f = async (a: number): Promise<void> => {};
const g = function () {};
export function h(a: number): number;
export function h(a: any) { return a }
let x = 1, y = 2;
export abstract class Ab { abstract z(): void }
declare function dd(): void;
declare global { interface Window { a: 1 } }
export { a, b as c };
export { d } from "./d";
import fs = require("fs");
const enum CE { A }
console.log(x);
