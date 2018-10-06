import React from 'react';
import PropTypes from 'prop-types';
import { withStyles } from '@material-ui/core/styles';
import Stepper from '@material-ui/core/Stepper';
import Step from '@material-ui/core/Step';
import StepLabel from '@material-ui/core/StepLabel';
import Typography from '@material-ui/core/Typography';

const styles = theme => ({
  root: {
    width: '90%',
  },
  button: {
    marginRight: theme.spacing.unit,
  },
  instructions: {
    marginTop: theme.spacing.unit,
    marginBottom: theme.spacing.unit,
  },
});

function getSteps(numResources, resource) {
  return [
    'Controller successfully installed',
    `${numResources} ${resource}s detected`,
    `Connect your first ${resource}`
  ];
}

class HorizontalLinearStepper extends React.Component {
  state = {
    activeStep: 0
  };

  render() {
    const { resource, numResources, classes } = this.props;
    const steps = getSteps(numResources, resource);
    const { activeStep } = this.state;

    return (
      <React.Fragment>
        <Typography>The service mesh was successfully installed!</Typography>
        <Stepper activeStep={activeStep}>
          {steps.map((label, i) => {
        const props = {};

        props.completed = i < steps.length - 1;

        return (
          <Step key={label} {...props}>
            <StepLabel>{label}</StepLabel>
          </Step>
        );
      })}
        </Stepper>
      </React.Fragment>
    );
  }
}

HorizontalLinearStepper.propTypes = {
  classes: PropTypes.object,
};

export default withStyles(styles)(HorizontalLinearStepper);
