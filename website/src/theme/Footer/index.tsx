import React from 'react';
import Footer from '@theme-original/Footer';
import type FooterType from '@theme/Footer';
import type { WrapperProps } from '@docusaurus/types';

import styles from './footer.module.css';

type Props = WrapperProps<typeof FooterType>;

export default function FooterWrapper(props) {
  const heart = require('@site/static/img/heart.svg').default;
  return (
    <>
      <section className={styles.awsFooter} >
        <p className={styles.awsFooterText}>Built with&ensp;
          <img alt="heart" className={styles.awsFooterTextHeart} />
          &ensp;at AWS</p>
      </section>
      <Footer {...props} />
    </>
  );
}