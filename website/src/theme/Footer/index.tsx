import React from 'react';
import Footer from '@theme-original/Footer';
import type FooterType from '@theme/Footer';
import type { WrapperProps } from '@docusaurus/types';

import styles from './footer.module.css';

type Props = WrapperProps<typeof FooterType>;

export default function FooterWrapper(props) {
  return (
    <>
      <section className={styles.kroFooter}>
        <p className={styles.kroFooterText}>
          Brought to you with ♥ by{' '}
          <a
            href="https://github.com/kubernetes/community/tree/master/sig-cloud-provider"
            target="_blank"
            rel="noopener noreferrer"
          >
            SIG Cloud Provider
          </a>
        </p>
      </section>
      <Footer {...props} />
      <script
        defer
        src='https://static.cloudflareinsights.com/beacon.min.js'
        data-cf-beacon='{"token": "85e669833a874cdd938772c6e459b33e"}'
      ></script>
    </>
  );
}
