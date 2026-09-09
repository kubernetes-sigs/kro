import React from 'react';
import { ProcessFlow, USER, API, KRO, type Step } from '../RGDProcessFlow';

const steps: Step[] = [
  { num: 1, label: 'Apply Graph',            from: USER, to: API },
  { num: 2, label: 'Watch Graphs',           from: KRO,  to: API,  kro: true },
  { num: 3, label: 'Compile',                from: KRO,  to: KRO,  kro: true, self: true },
  { num: 4, label: 'Read ref targets',       from: KRO,  to: API,  kro: true },
  { num: 5, label: 'Apply as ServiceAccount', from: KRO, to: API,  kro: true },
  { num: 6, label: 'Watch resources',        from: KRO,  to: API,  kro: true },
  { num: 7, label: 'Reconcile',              from: KRO,  to: KRO,  kro: true, self: true },
  { num: 8, label: 'Update status',          from: KRO,  to: API,  kro: true },
];

export default function GraphProcessFlow(): JSX.Element {
  return <ProcessFlow steps={steps} />;
}
